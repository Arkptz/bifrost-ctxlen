// Package plugin reports a request's estimated context size so Bifrost's
// routing rules can decide where to send it.
//
// Bifrost's routing rules are CEL expressions over a FIXED set of variables,
// built by one hardcoded cel.NewEnv() in the routing plugin; nothing lets a
// plugin register a new one. There is therefore no `context_length > 500000`
// to write, even though upstream has an unmerged PR adding exactly that.
//
// What a plugin CAN do is write into the request-headers map the routing plugin
// already reads to populate its `headers[...]` variable. So this measures the
// request, publishes the NUMBER, and leaves every threshold in the admin UI:
//
//	int(headers["x-ctx-tokens"]) > 500000   ->  a long-context provider
//	int(headers["x-ctx-tokens"]) > 900000   ->  something bigger still
//
// Publishing the count rather than a yes/no flag is deliberate. A boolean bakes
// one threshold into the plugin, so changing it — or adding a second tier —
// means editing the config, and every rule can only ever ask the one question
// the plugin already decided. A number lets rules ask their own questions, and
// as many as they like, with no plugin change at all. CEL's int() conversion on
// this build was verified against the live gateway before committing to it.
//
// The behaviour lives in this importable package rather than in the root
// `package main`, which is only the shim exporting symbols for plugin.Open.
// That keeps the door open to compiling this into a bifrost binary instead.
package plugin

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/maximhq/bifrost/core/schemas"
)

// Name is the plugin's system identifier. Bifrost keys its config row, its
// placement and its execution order on this, so it must stay stable.
const Name = "ctxlen"

const (
	// defaultHeader carries the estimated token count, as a decimal string —
	// header values are strings, and CEL's int() converts on the rule side.
	// The `x-` prefix marks it as non-standard, and the name is deliberately
	// specific: a generic one risks colliding with something a client sends.
	defaultHeader = "x-ctx-tokens"

	// bytesPerToken converts serialized request size to an approximate token
	// count. Four is the usual rule of thumb for English text and the same
	// estimate upstream's own draft implementation falls back to.
	//
	// A real tokenizer would be more accurate and much worse here: it would
	// mean a heavyweight dependency, per-model vocabularies, and — the part
	// that actually bites — any library also linked by the host widens the
	// shared-package surface that Go's plugin runtime version-checks. For a
	// coarse "is this request enormous" question, an estimate that is never
	// wrong by more than a factor of two is enough.
	bytesPerToken = 4
)

// Plugin measures requests and publishes the estimate.
//
// The header name is atomic because PUT /api/plugins/<name> rewrites the config
// of a LIVE plugin while it is serving requests: a plain struct field would be
// a data race.
type Plugin struct {
	header atomic.Value // string
}

// New returns a plugin with defaults applied. Init may override them.
func New() *Plugin {
	p := &Plugin{}
	p.header.Store(defaultHeader)
	return p
}

// Init applies the config stored in bifrost's `config_plugins.config_json`:
//
//	{"header": "x-ctx-tokens"}
//
// There is no threshold to configure: the plugin reports the measurement and
// the rules decide what counts as large, so a new tier is a new rule rather
// than a config change plus a restart.
//
// The value arrives as a raw map — the .so loader cannot unmarshal into a typed
// struct — so each field is type-asserted defensively. A malformed value is
// reported rather than ignored: silently falling back to a default makes a typo
// in the admin UI look exactly like the setting being honoured.
func (p *Plugin) Init(config any) error {
	if config == nil {
		return nil
	}

	cfg, ok := config.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: config must be an object, got %T", Name, config)
	}

	if raw, present := cfg["header"]; present {
		header, ok := raw.(string)
		if !ok {
			return fmt.Errorf("%s: header must be a string, got %T", Name, raw)
		}
		if header == "" {
			return fmt.Errorf("%s: header must not be empty", Name)
		}
		p.header.Store(header)
	}

	return nil
}

// Header returns the configured header name.
func (p *Plugin) Header() string {
	header, _ := p.header.Load().(string)
	return header
}

// EstimateTokens approximates the token count of a request.
//
// Only the message payload is measured: it dominates the size of any request
// big enough to matter, and it is the part that actually consumes the context
// window. Requests carrying no messages estimate to zero, which is correct —
// they cannot be large-context.
func (p *Plugin) EstimateTokens(req *schemas.BifrostRequest) int64 {
	if req == nil {
		return 0
	}

	var payload any
	switch {
	case req.ChatRequest != nil:
		payload = req.ChatRequest.Input
	case req.ResponsesRequest != nil:
		payload = req.ResponsesRequest.Input
	default:
		// Embeddings, transcription, image generation and the rest are not
		// context-window bound in the way this plugin cares about.
		return 0
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		// An unmarshallable payload is not this plugin's problem to report:
		// treating it as "not large" lets the request proceed and fail (or
		// succeed) on its own terms downstream.
		return 0
	}

	return int64(len(encoded)) / bytesPerToken
}

// PreRequestHook measures the request and publishes the estimate as a header.
//
// Registration must set "placement": "pre_builtin", otherwise this runs AFTER
// the routing plugin and the header arrives too late to affect anything.
//
// Two details matter more than they look:
//
// The header is written on EVERY request, "0" included. The map starts as a
// copy of the client's own headers, so a value written only for large requests
// would let any caller send their own count and route themselves. Overwriting
// unconditionally makes the client's value irrelevant.
//
// The map is REPLACED, not mutated. It is shared with other hooks and read
// concurrently, so an in-place write is a data race.
func (p *Plugin) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	if ctx == nil {
		return nil
	}

	value := strconv.FormatInt(p.EstimateTokens(req), 10)

	existing, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	headers := make(map[string]string, len(existing)+1)
	for k, v := range existing {
		headers[k] = v
	}
	// Keys are lowercased by the transport, and the routing engine lowercases
	// both sides before matching; keep that invariant.
	headers[p.Header()] = value

	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, headers)

	return nil
}

// Cleanup runs at shutdown. This plugin holds no resources.
func (p *Plugin) Cleanup() error { return nil }
