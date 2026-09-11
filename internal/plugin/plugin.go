// Package plugin flags large-context requests so Bifrost's routing rules can
// send them somewhere that handles them.
//
// Bifrost's routing rules are CEL expressions over a FIXED set of variables,
// built by one hardcoded cel.NewEnv() in the routing plugin; nothing lets a
// plugin register a new one. There is therefore no `context_length > 500000`
// to write, even though upstream has an unmerged PR adding exactly that.
//
// What a plugin CAN do is write into the request-headers map the routing plugin
// already reads to populate its `headers[...]` variable. So this measures the
// request, sets a header, and leaves the actual routing decision in the admin
// UI where it belongs:
//
//	headers["x-ctx-large"] == "1"   ->  some long-context provider
//
// The behaviour lives in this importable package rather than in the root
// `package main`, which is only the shim exporting symbols for plugin.Open.
// That keeps the door open to compiling this into a bifrost binary instead.
package plugin

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/maximhq/bifrost/core/schemas"
)

// Name is the plugin's system identifier. Bifrost keys its config row, its
// placement and its execution order on this, so it must stay stable.
const Name = "ctxlen"

const (
	// defaultThresholdTokens is where "large" starts. 500k sits above the 200k
	// window of the common Anthropic-family models, so anything past it is
	// already in territory a normal provider cannot serve.
	defaultThresholdTokens = 500_000

	// defaultHeader is the header written into the context for CEL to match.
	// The `x-` prefix marks it as non-standard, and the name is deliberately
	// specific: a generic one risks colliding with something a client sends.
	defaultHeader = "x-ctx-large"

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

// Plugin flags requests whose estimated size crosses a threshold.
//
// Every field a hook reads is atomic because PUT /api/plugins/<name> rewrites
// the config of a LIVE plugin while it is serving requests: plain struct fields
// would be a data race.
type Plugin struct {
	threshold atomic.Int64
	header    atomic.Value // string
}

// New returns a plugin with defaults applied. Init may override them.
func New() *Plugin {
	p := &Plugin{}
	p.threshold.Store(defaultThresholdTokens)
	p.header.Store(defaultHeader)
	return p
}

// Init applies the config stored in bifrost's `config_plugins.config_json`:
//
//	{"threshold_tokens": 500000, "header": "x-ctx-large"}
//
// The value arrives as a raw map — the .so loader cannot unmarshal into a typed
// struct — so each field is type-asserted defensively. JSON numbers decode as
// float64, hence the conversion. A malformed value is reported rather than
// ignored: silently falling back to a default makes a typo in the admin UI look
// exactly like the setting being honoured.
func (p *Plugin) Init(config any) error {
	if config == nil {
		return nil
	}

	cfg, ok := config.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: config must be an object, got %T", Name, config)
	}

	if raw, present := cfg["threshold_tokens"]; present {
		threshold, ok := raw.(float64)
		if !ok {
			return fmt.Errorf("%s: threshold_tokens must be a number, got %T", Name, raw)
		}
		if threshold <= 0 {
			return fmt.Errorf("%s: threshold_tokens must be positive, got %v", Name, threshold)
		}
		p.threshold.Store(int64(threshold))
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

// Threshold returns the configured token threshold.
func (p *Plugin) Threshold() int64 { return p.threshold.Load() }

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

// PreRequestHook measures the request and records the verdict as a header.
//
// Registration must set "placement": "pre_builtin", otherwise this runs AFTER
// the routing plugin and the header arrives too late to affect anything.
//
// Two details matter more than they look:
//
// The header is written on EVERY request, including a "0" for small ones. The
// map starts as a copy of the client's own headers, so a value set only when
// the request is large would let any caller send `x-ctx-large: 1` and route
// itself. Overwriting unconditionally makes the client's value irrelevant.
//
// The map is REPLACED, not mutated. It is shared with other hooks and read
// concurrently, so an in-place write is a data race.
func (p *Plugin) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	if ctx == nil {
		return nil
	}

	value := "0"
	if p.EstimateTokens(req) >= p.threshold.Load() {
		value = "1"
	}

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
