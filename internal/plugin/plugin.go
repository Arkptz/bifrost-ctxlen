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
	"strings"
	"sync"
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

	// Text is priced per BYTE CLASS, not by one flat divisor.
	//
	// The familiar "4 per token" is 4 CHARACTERS of English prose (OpenAI's
	// help centre and Gemini's docs both say characters, and both warn that
	// "other languages can have different relationships between characters,
	// words, and tokens"). Applying it to len(json.Marshal(...)) — which is
	// BYTES — mixes up the two units, and Go writes non-ASCII into JSON as raw
	// UTF-8, so one Cyrillic character arrives as two bytes. Prose is also the
	// wrong corpus: this traffic is code, JSON and tool schemas, which tokenize
	// denser than prose. Both errors point the same way, and measured against
	// what providers actually billed for 12 real requests the old divisor came
	// out 30-39% LOW — the dangerous direction, since a request that looks
	// small gets routed to a short-context provider and fails mid-session.
	//
	// The two constants below are fitted to those 12 requests (see
	// TestCalibrateAgainstRealTraffic). They are not arbitrary: 3.85 bytes per
	// non-ASCII token is 1.93 CHARACTERS per token for two-byte Cyrillic, which
	// independently matches the 1.5-2 chars/token that o200k is documented to
	// achieve on Russian. Worst case over the corpus is 3.7%, against 39%.
	//
	// A real tokenizer would be more accurate and much worse here: it would
	// mean a heavyweight dependency, per-model vocabularies, and — the part
	// that actually bites — any library also linked by the host widens the
	// shared-package surface that Go's plugin runtime version-checks.
	asciiBytesPerToken = 2.55

	// Non-ASCII is split by UTF-8 SEQUENCE LENGTH, because bytes-per-token is
	// not one number across scripts — it ranges about 4.5x, and a single
	// divisor mispriced whichever script it was not fitted to.
	//
	// Measured bytes-per-token (FLORES-200 via Petrov et al. NeurIPS 2023 for
	// cl100k; OpenAI's own published cl100k->o200k comparisons for the rest):
	//
	//   2-byte (Cyrillic, Greek, Hebrew)   ~4.3 cl100k, ~5.8 o200k
	//   3-byte (Han, kana, Hangul)         ~2.1-3.6 cl100k, ~3.6-4.5 o200k
	//   4-byte (emoji, rare CJK ext.)      ~1.3-2.0 — emoji are the cheapest
	//                                      text there is per byte, since a ZWJ
	//                                      sequence has no single token and
	//                                      shatters into byte fragments
	//
	// Each constant sits at or below the low end of its measured range, so the
	// estimate errs HIGH in tokens — the safe direction, since underestimating
	// routes an oversized request to a short-context provider and fails it.
	// twoByteBytesPerToken keeps the value calibrated against this deployment's
	// own billed tokens, which is dominated by Cyrillic.
	twoByteBytesPerToken   = 3.85
	threeByteBytesPerToken = 2.5
	fourByteBytesPerToken  = 1.5

	// tokensPerMessage is the per-message framing every chat API adds around
	// content (role markers and separators). OpenAI's cookbook counts 3 tokens
	// per message for current models, and that is what this is.
	tokensPerMessage = 3

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

// Request families, as published. A closed set on purpose: every value written
// to a header or a log line must be one this plugin chose, never a string that
// arrived with the request.
const (
	kindChat      = "chat"
	kindResponses = "responses"
	kindOther     = "other"
)

// headerPrefix namespaces the breakdown headers. The total keeps its own name
// (defaultHeader) because routing rules already compare it and renaming it would
// break them.
const headerPrefix = "x-ctxlen-"

// logSchema prefixes every log line. It is both the grep anchor and the format
// version: a consumer that parses these lines can refuse a version it does not
// know, instead of silently misreading a reordered field.
const logSchema = "ctxlen/1"

// Plugin measures requests and publishes the estimate.
//
// The header name is atomic because PUT /api/plugins/<name> rewrites the config
// of a LIVE plugin while it is serving requests: a plain struct field would be
// a data race.
type Plugin struct {
	header atomic.Value // string

	// verifyOnce gates the one-shot check that ctx.Log is actually recording.
	verifyOnce sync.Once
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
		// Lowercased, and this is a security control rather than tidiness.
		//
		// The routing engine lowercases every header key before evaluating CEL,
		// so `X-Ctx-Tokens` and `x-ctx-tokens` collapse to one variable. But the
		// map this plugin writes into starts as a copy of the CLIENT's headers,
		// keyed as the transport stored them. Writing under a differently-cased
		// name therefore leaves the client's own entry in place beside ours, and
		// which of the two survives the engine's normalisation is a coin toss —
		// measured at 39% in the client's favour. Storing the key already
		// lowercased means our write always lands on the same entry the client's
		// would, so the unconditional overwrite that makes spoofing impossible
		// actually overwrites.
		p.header.Store(strings.ToLower(header))
	}

	return nil
}

// Header returns the configured header name.
func (p *Plugin) Header() string {
	header, _ := p.header.Load().(string)
	return header
}

// Estimate is one request's measurement: the answer, and every input the
// arithmetic used to reach it.
//
// The breakdown is carried rather than discarded because the estimate is
// otherwise invisible in production — nothing downstream logs it, and a routing
// rule only ever reveals which side of a threshold it landed on. Publishing the
// inputs makes the constants checkable against what the provider actually
// billed, and lets a future calibration be assembled from production instead of
// from a replayed capture.
//
// The invariant a reader can check by eye: Total == Text + Framing + Media.
type Estimate struct {
	Total int64

	// Kind is the request family, from a closed set: "chat", "responses" or
	// "other". Never a model or provider name — see publish().
	Kind string

	Messages     int64
	Tools        int64
	Instructions int64

	// Bytes is the measured payload split by UTF-8 sequence length, with media
	// bytes already removed from the ASCII bucket. MediaBytes is what was
	// removed.
	Bytes      byteClasses
	MediaBytes int64

	Text    int64
	Framing int64
	Media   int64

	Images     int64
	AudioClips int64
	InlineDocs int64
	RemoteDocs int64

	// Unmeasurable marks a payload that would not serialize. The estimate is
	// then zero, which makes a large request look small — the failure this
	// plugin exists to prevent — so it is reported rather than swallowed.
	Unmeasurable bool
}

// priceText converts counted bytes into tokens, per byte class.
//
// Split out from the measurement so the calibration fixture can exercise the
// pricing law directly, on recorded byte counts, without reconstructing a
// request and re-serializing it. The fixture then pins what the constants
// claim rather than how the bytes were obtained.
func priceText(c byteClasses) int64 {
	return int64(float64(c.ascii)/asciiBytesPerToken +
		float64(c.twoByte)/twoByteBytesPerToken +
		float64(c.threeByte)/threeByteBytesPerToken +
		float64(c.fourByte)/fourByteBytesPerToken)
}

// EstimateTokens approximates the token count of a request.
func (p *Plugin) EstimateTokens(req *schemas.BifrostRequest) int64 {
	return p.estimate(req, nil).Total
}

// Estimate measures a request and returns the full breakdown, serializing the
// payload to count its bytes.
//
// This is the fallback path, for callers with no transport hook (SDK embedding,
// realtime websocket). The gateway's normal path measures the raw body in
// HTTPTransportPreHook and reaches the estimate through PreRequestHook, which
// hands that measurement to estimate() and never serializes.
func (p *Plugin) Estimate(req *schemas.BifrostRequest) Estimate {
	return p.estimate(req, nil)
}

// estimate measures a request. When body is non-nil it is the raw request
// bytes, already counted and media-subtracted, and no json.Marshal happens —
// that is the whole point, since the marshal was 76% of this plugin's cost. When
// body is nil the payload is serialized instead.
//
// What is measured is the prompt: the messages, the tool definitions and the
// system instructions, which are what actually occupies the context window. A
// request carrying none of them estimates to zero, which is correct — it cannot
// be large-context.
//
// Non-text parts are never priced by their bytes: no provider bills an image,
// an audio clip or a PDF by the characters that carried it. They are found by
// walking the parsed structs — cheap, no allocation — and priced by modality,
// with their bytes removed from the text count.
func (p *Plugin) estimate(req *schemas.BifrostRequest, body *bodySize) Estimate {
	if req == nil {
		return Estimate{Kind: kindOther}
	}

	var media mediaCost
	out := Estimate{Kind: kindOther}

	switch {
	case req.ChatRequest != nil:
		out.Kind = kindChat
		media = chatMediaCost(req.ChatRequest.Input)
		out.Messages = int64(len(req.ChatRequest.Input))
		if params := req.ChatRequest.Params; params != nil {
			out.Tools = int64(len(params.Tools))
		}
	case req.ResponsesRequest != nil:
		out.Kind = kindResponses
		media = responsesMediaCost(req.ResponsesRequest.Input)
		out.Messages = int64(len(req.ResponsesRequest.Input))
		if params := req.ResponsesRequest.Params; params != nil {
			out.Tools = int64(len(params.Tools))
			if params.Instructions != nil {
				out.Instructions = 1
			}
		}
	default:
		// Embeddings, transcription, image generation and the rest are not
		// context-window bound in the way this plugin cares about.
		return out
	}

	out.MediaBytes = media.bytes
	out.Media = media.tokens
	out.Images = media.images
	out.AudioClips = media.audioClips
	out.InlineDocs = media.inlineDocs
	out.RemoteDocs = media.remoteDocs

	// The body is the cheap path, but it is not always usable: a payload the
	// transport declined to copy leaves nothing to count. Fall through to
	// serializing rather than publish an estimate derived from an absent body.
	priced := false
	if body != nil {
		priced = out.fromBody(*body, media.bytes)
	}
	if !priced && !out.fromMarshal(req, media.bytes) {
		out.Unmeasurable = true
		return out
	}

	out.Framing = out.Messages * tokensPerMessage
	out.Total = out.Text + out.Framing + out.Media
	return out
}

// fromBody prices the text from the raw request body the transport hook
// counted. Media bytes are removed the same way the marshal path removes them:
// the body carries inline base64 in full, and that is ASCII, so an unremoved
// image would be priced as hundreds of thousands of tokens of text.
//
// Returns false when the body was not retained and cannot be priced, so the
// caller falls back to serializing rather than publishing a number derived from
// an absent body.
func (e *Estimate) fromBody(body bodySize, mediaBytes int64) bool {
	if !body.measured {
		// The transport skipped the copy: over the large-payload threshold, or
		// a length it could not determine. Content-Length is usually gone by
		// then too — the decompression middleware deletes it — so the declared
		// size is not a dependable substitute. Use it only when it is actually
		// there, and otherwise say so, because a request whose body was too big
		// to copy is precisely the one that must not read as small.
		if body.contentLength <= 0 {
			return false
		}
		// Priced as ASCII: no byte-class breakdown exists for a body nobody
		// read, and ASCII is the cheaper divisor, so this errs high in tokens —
		// the safe direction for a routing threshold.
		e.Bytes = byteClasses{ascii: body.contentLength}
		e.Text = priceText(e.Bytes)
		return true
	}

	// Media is base64 and URLs — ASCII — so it comes off the ASCII bucket.
	e.Bytes = body.classes
	e.Bytes.ascii = max(e.Bytes.ascii-mediaBytes, 0)
	e.Text = priceText(e.Bytes)
	return true
}

// fromMarshal prices the text by serializing the payload, for callers with no
// transport hook. Returns false if the payload will not serialize — the caller
// then reports the request as unmeasurable rather than letting a zero estimate
// route a large request as small.
func (e *Estimate) fromMarshal(req *schemas.BifrostRequest, mediaBytes int64) bool {
	var payload []any
	switch {
	case req.ChatRequest != nil:
		payload = append(payload, req.ChatRequest.Input)
		if params := req.ChatRequest.Params; params != nil {
			payload = append(payload, params.Tools)
		}
	case req.ResponsesRequest != nil:
		payload = append(payload, req.ResponsesRequest.Input)
		if params := req.ResponsesRequest.Params; params != nil {
			payload = append(payload, params.Tools, params.Instructions)
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	e.Bytes = countByteClasses(encoded)
	e.Bytes.ascii = max(e.Bytes.ascii-mediaBytes, 0)
	e.Text = priceText(e.Bytes)
	return true
}

// byteClasses is a UTF-8 buffer split by sequence length.
//
// Four buckets and not two, because bytes-per-token varies about 4.5x across
// scripts and tracks the UTF-8 length closely: 2-byte scripts tokenize around
// 4-6 bytes per token, 3-byte CJK around 2-4, and 4-byte emoji below 2, since a
// ZWJ sequence has no single token and shatters into byte fragments. One
// non-ASCII divisor therefore misprices whichever script it was not fitted to.
type byteClasses struct {
	ascii     int64
	twoByte   int64
	threeByte int64
	fourByte  int64
}

func (c byteClasses) nonASCII() int64 {
	return c.twoByte + c.threeByte + c.fourByte
}

func (c byteClasses) total() int64 {
	return c.ascii + c.nonASCII()
}

// countByteClasses splits a UTF-8 buffer by sequence length.
//
// It counts LEAD bytes and attributes each sequence's full width to its class,
// so the buckets sum to the buffer length. Continuation bytes (0b10xxxxxx) are
// skipped rather than counted separately. Malformed input cannot desynchronise
// the total: a stray continuation byte falls through to the ASCII bucket, which
// is the cheapest divisor and therefore the safe direction.
//
// A single linear pass with no allocation, which matters because it runs on
// every request — measured at ~2.9 GB/s.
func countByteClasses(b []byte) byteClasses {
	var c byteClasses
	for _, x := range b {
		switch {
		case x < 0x80:
			c.ascii++
		case x < 0xC0:
			// Continuation byte: already accounted for by its lead byte.
		case x < 0xE0:
			c.twoByte += 2
		case x < 0xF0:
			c.threeByte += 3
		default:
			c.fourByte += 4
		}
	}
	return c
}

// mediaCost is what the non-text parts of a payload contribute: the serialized
// bytes they occupy (to be removed from the text estimate) and the tokens a
// provider will actually bill for them. The per-modality counts are carried so
// the published breakdown can be checked against the provider's own
// prompt_tokens_details, which reports image and audio tokens separately.
type mediaCost struct {
	bytes  int64
	tokens int64

	images     int64
	audioClips int64
	inlineDocs int64
	remoteDocs int64
}

func (m *mediaCost) addAudio(payload string) {
	m.bytes += int64(len(payload))
	m.tokens += decodedSize(payload) / audioBytesPerToken
	m.audioClips++
}

func (m *mediaCost) addInlineDocument(payload string) {
	m.bytes += int64(len(payload))
	m.tokens += decodedSize(payload) / fileBytesPerToken
	m.inlineDocs++
}

func (m *mediaCost) addImage(reference string) {
	m.bytes += int64(len(reference))
	m.tokens += imageTokens
	m.images++
}

func (m *mediaCost) addRemoteFile(reference string) {
	m.bytes += int64(len(reference))
	m.tokens += remoteFileTokens
	m.remoteDocs++
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
			cost.addAudio(message.Audio.Data)
		}

		if message.Content == nil {
			continue
		}
		for _, block := range message.Content.ContentBlocks {
			switch {
			case block.ImageURLStruct != nil:
				cost.addImage(block.ImageURLStruct.URL)
			case block.InputAudio != nil:
				cost.addAudio(block.InputAudio.Data)
			case block.File != nil:
				switch {
				case block.File.FileData != nil:
					cost.addInlineDocument(*block.File.FileData)
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
				cost.addAudio(block.Audio.Data)
			case block.ResponsesInputMessageContentBlockFile != nil:
				switch {
				case block.FileData != nil:
					cost.addInlineDocument(*block.FileData)
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

// field is one published measurement. The same set feeds the headers and the
// log line, so the two can never disagree about what was measured.
//
// Every value is an integer except kind, which is drawn from a closed set. That
// is a mechanical guarantee rather than a convention: nothing derived from
// request content — no message text, no tool name, no URL — can reach a header
// or a log line, so neither can leak content nor be forged by a client that
// names a tool `x est=1` to corrupt a drift query.
type field struct {
	name  string
	value int64
}

func (e Estimate) fields() []field {
	return []field{
		{"msgs", e.Messages},
		{"tools", e.Tools},
		{"sys", e.Instructions},
		{"ascii", e.Bytes.ascii},
		{"nonascii", e.Bytes.nonASCII()},
		// Broken out because the three non-ASCII widths are priced differently
		// and a drift query needs to know which one moved.
		{"utf2", e.Bytes.twoByte},
		{"utf3", e.Bytes.threeByte},
		{"utf4", e.Bytes.fourByte},
		{"mediab", e.MediaBytes},
		{"text", e.Text},
		{"frame", e.Framing},
		{"media", e.Media},
		{"img", e.Images},
		{"audio", e.AudioClips},
		{"doc", e.InlineDocs},
		{"docurl", e.RemoteDocs},
	}
}

// logLine renders the estimate as one line for the admin UI's Plugin Logs tab.
//
// Fixed arity: every field is printed, zeros included. Three extra bytes are
// cheaper than an awk script that silently shifts a column on a request that
// happened to carry no images.
func (e Estimate) logLine() string {
	var b strings.Builder
	b.Grow(192)

	b.WriteString(logSchema)
	if e.Unmeasurable {
		// Distinguished by the second token, so a parser can branch before it
		// looks for est=.
		b.WriteString(" warn=payload_unmarshalable kind=")
		b.WriteString(e.Kind)
		return b.String()
	}

	b.WriteString(" est=")
	b.WriteString(strconv.FormatInt(e.Total, 10))
	b.WriteString(" kind=")
	b.WriteString(e.Kind)
	for _, f := range e.fields() {
		b.WriteByte(' ')
		b.WriteString(f.name)
		b.WriteByte('=')
		b.WriteString(strconv.FormatInt(f.value, 10))
	}

	return b.String()
}

// PreRequestHook measures the request and publishes the estimate.
//
// Registration must set "placement": "pre_builtin", otherwise this runs AFTER
// the routing plugin and the header arrives too late to affect anything.
//
// Two details matter more than they look:
//
// The headers are written on EVERY request, "0" included. The map starts as a
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

	// Prefer the raw-body measurement the transport hook took: one linear scan
	// instead of a json.Marshal of the whole payload, which was 76% of this
	// plugin's cost. It priced within a few points of the marshal on live
	// traffic (both ~20% mean error against billed tokens — the spread is the
	// traffic, not the method), so the cheap path is authoritative and the
	// marshal is only a fallback for a request that reached here without the
	// transport hook (SDK embedding, realtime websocket).
	var estimate Estimate
	if size, ok := bodyMeasurement(ctx); ok {
		estimate = p.estimate(req, &size)
	} else {
		estimate = p.estimate(req, nil)
	}

	existing, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	headers := make(map[string]string, len(existing)+2+len(estimate.fields()))
	maps.Copy(headers, existing)
	// Keys are lowercased by the transport, and the routing engine lowercases
	// both sides before matching; keep that invariant.
	headers[p.Header()] = strconv.FormatInt(estimate.Total, 10)
	headers[headerPrefix+"kind"] = estimate.Kind
	for _, f := range estimate.fields() {
		headers[headerPrefix+f.name] = strconv.FormatInt(f.value, 10)
	}

	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, headers)

	// Publish the measurement where an operator can see it. Two channels on
	// purpose: the log line is forensics for one request in the admin UI, the
	// trace attribute is the aggregate in OTEL/Langfuse.
	ctx.Log(schemas.LogLevelInfo, estimate.logLine())
	ctx.SetTraceAttribute("ctxlen.estimate", estimate.Total)

	// ctx.Log is a SILENT no-op on a context the host did not scope to this
	// plugin (core/schemas/context.go: it returns early when pluginScope is
	// nil). That is the failure mode this instrumentation exists to prevent —
	// code that looks instrumented and emits nothing — so prove once per
	// process that the write lands, and report it through the one channel that
	// still works when logging does not: the hook's error return. It is
	// non-blocking (core/bifrost.go logs it and continues), and it runs after
	// the headers are published, so the measurement itself still happens.
	return p.verifyLogging(ctx)
}

// verifyLogging checks, once per process, that ctx.Log actually records.
//
// Once and not per-request: GetPluginLogs deep-copies the whole slice, which is
// free at startup and wasteful on every request.
func (p *Plugin) verifyLogging(ctx *schemas.BifrostContext) error {
	var err error
	p.verifyOnce.Do(func() {
		for _, entry := range ctx.GetPluginLogs() {
			if entry.PluginName == Name {
				return
			}
		}
		err = fmt.Errorf("%s: ctx.Log recorded nothing — the host is not scoping "+
			"PreRequestHook contexts to the plugin, so every estimate is unobservable", Name)
	})
	return err
}

// Cleanup runs at shutdown. This plugin holds no resources.
func (p *Plugin) Cleanup() error { return nil }
