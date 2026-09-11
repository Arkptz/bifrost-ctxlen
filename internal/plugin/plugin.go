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
	"maps"
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

	// bytesPerToken converts serialized TEXT size to an approximate token
	// count. Four is the usual rule of thumb for English text and the same
	// estimate upstream's own draft implementation falls back to
	// (core/providers/bedrock/count_tokens.go uses the same constant).
	//
	// A real tokenizer would be more accurate and much worse here: it would
	// mean a heavyweight dependency, per-model vocabularies, and — the part
	// that actually bites — any library also linked by the host widens the
	// shared-package surface that Go's plugin runtime version-checks. For a
	// coarse "is this request enormous" question, an estimate that is never
	// wrong by more than a factor of two is enough.
	bytesPerToken = 4

	// imageTokens is what one image part is charged, whatever its bytes.
	//
	// Providers bill the DECODED image by its pixel dimensions, never by the
	// characters that carried it: OpenAI charges 85 tokens at detail=low and
	// 765 for a 1024x1024 at detail=high (max 1445 for the tile family),
	// Anthropic ceil(w/28)*ceil(h/28) capped at 1568 on the standard tier,
	// Gemini 258 per 768px tile or a flat 1120 on Gemini 3. Sizing the request
	// by bytes therefore misjudges images in BOTH directions: a 1 MB inline
	// image is ~1.4 MB of base64 and would score ~350k tokens instead of ~1k,
	// while an 80-character https:// URL would score ~20 for the same picture.
	// The first inflation is what makes a byte-counting estimate dangerous —
	// one screenshot routes a small request to a long-context provider.
	//
	// 1600 is the top of the documented range, so the estimate errs high on
	// images rather than letting a genuinely long request look small.
	imageTokens = 1600

	// audioBytesPerToken converts DECODED audio bytes to tokens. Providers
	// bill audio per second (OpenAI Realtime 10 tokens/s, Gemini 32/s); the
	// request carries no duration, so this assumes ~16 kB/s (128 kbps, typical
	// mp3) and charges the higher of the two rates: 16000/32 = 500 bytes per
	// token.
	audioBytesPerToken = 500

	// fileBytesPerToken converts DECODED document bytes to tokens. Anthropic
	// documents 1500-3000 text tokens per PDF page and ~50 kB is an ordinary
	// page, which lands near 20 bytes per token.
	fileBytesPerToken = 20

	// remoteFileTokens is charged for a document referenced by URL or file id.
	// Its size is not in the request at all, so this is one page's worth
	// (Anthropic's documented per-page range) and a deliberate UNDERestimate
	// for a large remote document — nothing in the request could tell us more.
	remoteFileTokens = 2000

	// base64Numerator/base64Denominator recover the decoded size of a base64
	// payload without decoding it: 4 encoded characters carry 3 bytes.
	base64Numerator   = 3
	base64Denominator = 4
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
//
// Unknown keys pass without complaint on purpose: the same object is where a
// host reads its own per-plugin settings, so rejecting what this plugin does
// not recognise would reject the host's.
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
// What is measured is the prompt: the messages, the tool definitions and the
// system instructions, which are what actually occupies the context window. A
// request carrying none of them estimates to zero, which is correct — it cannot
// be large-context.
//
// Text is measured by its serialized size, but non-text parts are NOT: no
// provider bills an image, an audio clip or a PDF by the characters that
// carried it. Those parts are subtracted from the byte count and priced by
// modality instead — see mediaCost.
func (p *Plugin) EstimateTokens(req *schemas.BifrostRequest) int64 {
	if req == nil {
		return 0
	}

	// Tool definitions and the system instructions are part of the prompt and
	// are frequently the LARGER half of it: an agent client sends its whole
	// toolset on every turn, so measuring only the messages under-reports the
	// requests this plugin exists to catch.
	var payload []any
	var media mediaCost
	switch {
	case req.ChatRequest != nil:
		payload = append(payload, req.ChatRequest.Input)
		if params := req.ChatRequest.Params; params != nil {
			payload = append(payload, params.Tools)
		}
		media = chatMediaCost(req.ChatRequest.Input)
	case req.ResponsesRequest != nil:
		payload = append(payload, req.ResponsesRequest.Input)
		if params := req.ResponsesRequest.Params; params != nil {
			payload = append(payload, params.Tools, params.Instructions)
		}
		media = responsesMediaCost(req.ResponsesRequest.Input)
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

	// Clamped at zero: only reachable if JSON escaping made the encoded form
	// shorter than the raw strings it contains, which it cannot — but a
	// negative would subtract from the media estimate rather than fail loudly.
	textBytes := max(int64(len(encoded))-media.bytes, 0)

	return textBytes/bytesPerToken + media.tokens
}

// mediaCost is what the non-text parts of a payload contribute: the serialized
// bytes they occupy (to be removed from the text estimate) and the tokens a
// provider will actually bill for them.
type mediaCost struct {
	bytes  int64
	tokens int64
}

func (m *mediaCost) addInline(payload string, bytesPerToken int64) {
	m.bytes += int64(len(payload))
	m.tokens += decodedSize(payload) / bytesPerToken
}

func (m *mediaCost) addImage(reference string) {
	m.bytes += int64(len(reference))
	m.tokens += imageTokens
}

func (m *mediaCost) addRemoteFile(reference string) {
	m.bytes += int64(len(reference))
	m.tokens += remoteFileTokens
}

// decodedSize is the byte length a base64 payload decodes to. A payload that is
// not base64 at all (a plain-text file block, a data: URL prefix) is measured as
// itself plus a quarter, which is close enough for a size estimate.
func decodedSize(payload string) int64 {
	return int64(len(payload)) * base64Numerator / base64Denominator
}

func chatMediaCost(messages []schemas.ChatMessage) mediaCost {
	var cost mediaCost

	for _, message := range messages {
		if message.ChatAssistantMessage != nil && message.Audio != nil {
			// Audio the model produced, replayed as history: it occupies the
			// context window exactly like audio the client sent.
			cost.addInline(message.Audio.Data, audioBytesPerToken)
		}

		if message.Content == nil {
			continue
		}
		for _, block := range message.Content.ContentBlocks {
			switch {
			case block.ImageURLStruct != nil:
				cost.addImage(block.ImageURLStruct.URL)
			case block.InputAudio != nil:
				cost.addInline(block.InputAudio.Data, audioBytesPerToken)
			case block.File != nil:
				switch {
				case block.File.FileData != nil:
					cost.addInline(*block.File.FileData, fileBytesPerToken)
				case block.File.FileURL != nil:
					cost.addRemoteFile(*block.File.FileURL)
				case block.File.FileID != nil:
					cost.addRemoteFile(*block.File.FileID)
				}
			}
		}
	}

	return cost
}

func responsesMediaCost(messages []schemas.ResponsesMessage) mediaCost {
	var cost mediaCost

	for _, message := range messages {
		if message.Content == nil {
			continue
		}
		for _, block := range message.Content.ContentBlocks {
			switch {
			case block.ResponsesInputMessageContentBlockImage != nil:
				if block.ImageURL != nil {
					cost.addImage(*block.ImageURL)
				} else {
					cost.addImage("")
				}
			case block.Audio != nil:
				cost.addInline(block.Audio.Data, audioBytesPerToken)
			case block.ResponsesInputMessageContentBlockFile != nil:
				switch {
				case block.FileData != nil:
					cost.addInline(*block.FileData, fileBytesPerToken)
				case block.FileURL != nil:
					cost.addRemoteFile(*block.FileURL)
				case block.FileID != nil:
					cost.addRemoteFile(*block.FileID)
				}
			}
		}
	}

	return cost
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
	maps.Copy(headers, existing)
	// Keys are lowercased by the transport, and the routing engine lowercases
	// both sides before matching; keep that invariant.
	headers[p.Header()] = value

	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, headers)

	return nil
}

// Cleanup runs at shutdown. This plugin holds no resources.
func (p *Plugin) Cleanup() error { return nil }
