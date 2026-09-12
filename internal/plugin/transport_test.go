package plugin

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

func chatBody(t *testing.T, req *schemas.BifrostRequest) []byte {
	t.Helper()
	payload := map[string]any{
		"model":    "claude-opus-5",
		"messages": req.ChatRequest.Input,
	}
	if req.ChatRequest.Params != nil {
		payload["tools"] = req.ChatRequest.Params.Tools
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runWithTransport threads a request through both hooks the way the gateway
// does: the transport hook measures the raw body, then PreRequestHook publishes.
func runWithTransport(t *testing.T, p *Plugin, req *schemas.BifrostRequest, body []byte, path string) map[string]string {
	t.Helper()

	root := schemas.NewBifrostContext(t.Context(), time.Now())

	name := Name
	transportCtx := root.WithPluginScope(&name)
	httpReq := &schemas.HTTPRequest{
		Path:    path,
		Headers: map[string]string{"content-length": strconv.Itoa(len(body))},
		Body:    body,
	}
	if _, err := p.HTTPTransportPreHook(transportCtx, httpReq); err != nil {
		t.Fatalf("HTTPTransportPreHook() = %v", err)
	}
	transportCtx.ReleasePluginScope()

	preCtx := root.WithPluginScope(&name)
	if err := p.PreRequestHook(preCtx, req); err != nil {
		t.Fatalf("PreRequestHook() = %v", err)
	}
	headers, _ := preCtx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return headers
}

func TestTransportHookMeasuresTheBody(t *testing.T) {
	t.Parallel()

	p := New()
	req := chatRequest(10_000)
	body := chatBody(t, req)

	// x-ctx-tokens is now derived from the body the transport hook measured.
	headers := runWithTransport(t, p, req, body, "/v1/chat/completions")
	bodyRouted, err := strconv.ParseInt(headers[p.Header()], 10, 64)
	if err != nil {
		t.Fatalf("%s = %q, not an integer: %v", p.Header(), headers[p.Header()], err)
	}
	if bodyRouted == 0 {
		t.Fatal("estimate = 0: the transport hook did not measure the body")
	}

	// The body path and the marshal fallback measure slightly different things
	// (the body carries top-level params), but for a text-only request they
	// land within a few points — that closeness is the premise of the swap.
	marshalOnly := p.EstimateTokens(req)
	ratio := float64(bodyRouted) / float64(marshalOnly)
	if ratio < 0.9 || ratio > 1.2 {
		t.Errorf("body-routed=%d marshal=%d (ratio %.3f): the two paths disagree too much",
			bodyRouted, marshalOnly, ratio)
	}
}

// TestBodyEstimateSubtractsInlineMedia is the regression for a real defect: the
// raw body carries inline base64 in full, and counting it as text priced a 1 MB
// screenshot as ~350k tokens. The body path must subtract media the same way the
// marshal path does.
func TestBodyEstimateSubtractsInlineMedia(t *testing.T) {
	t.Parallel()

	// One image as a ~1 MB base64 data URL, plus a short text message.
	dataURL := "data:image/png;base64," + strings.Repeat("A", (1<<20)*4/3)
	req := &schemas.BifrostRequest{ChatRequest: &schemas.BifrostChatRequest{
		Input: []schemas.ChatMessage{
			newTextMessage("describe this"),
			{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{
				ContentBlocks: []schemas.ChatContentBlock{{
					Type:           schemas.ChatContentBlockTypeImage,
					ImageURLStruct: &schemas.ChatInputImage{URL: dataURL},
				}},
			}},
		},
	}}
	body := chatBody(t, req)

	p := New()
	headers := runWithTransport(t, p, req, body, "/v1/chat/completions")
	routed, _ := strconv.ParseInt(headers[p.Header()], 10, 64)

	// The image is ~1 MB of base64 ASCII. Priced as text it would be hundreds
	// of thousands of tokens; priced by modality it is ~1.6k plus the tiny text.
	if routed > 4*imageTokens {
		t.Errorf("estimate=%d for a 1 MB inline image: media bytes were counted as text", routed)
	}
	// And it must agree with the marshal fallback, which also subtracts media.
	marshalOnly := p.EstimateTokens(req)
	ratio := float64(routed) / float64(max(marshalOnly, 1))
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("body=%d marshal=%d: the two media accountings disagree", routed, marshalOnly)
	}
}

func TestTransportHookSkipsNonInferencePaths(t *testing.T) {
	t.Parallel()

	p := New()
	root := schemas.NewBifrostContext(t.Context(), time.Now())
	name := Name
	ctx := root.WithPluginScope(&name)

	httpReq := &schemas.HTTPRequest{
		Path: "/api/routing/rules",
		Body: []byte(strings.Repeat("x", 100_000)),
	}
	if _, err := p.HTTPTransportPreHook(ctx, httpReq); err != nil {
		t.Fatalf("HTTPTransportPreHook() = %v", err)
	}
	if _, ok := bodyMeasurement(ctx); ok {
		t.Error("an admin API path was measured; only inference paths should be")
	}
}

// TestUnretainedBodyFallsBackToSerializing is the regression for the worst
// underestimate this plugin can produce.
//
// The decompression middleware DELETES Content-Length before the transport
// decides whether to retain the body, so a gzipped or chunked request arrives
// with neither. Treating that as an empty body published a three-token estimate
// for a payload worth hundreds of thousands — and an underestimate is what
// routes a huge request to a short-context provider.
func TestUnretainedBodyFallsBackToSerializing(t *testing.T) {
	t.Parallel()

	p := New()
	req := chatRequest(50_000)

	root := schemas.NewBifrostContext(t.Context(), time.Now())
	name := Name
	transportCtx := root.WithPluginScope(&name)

	// No body, no content-length: exactly what gzip/chunked leaves behind.
	if _, err := p.HTTPTransportPreHook(transportCtx, &schemas.HTTPRequest{
		Path:    "/v1/chat/completions",
		Headers: map[string]string{},
		Body:    nil,
	}); err != nil {
		t.Fatalf("HTTPTransportPreHook() = %v", err)
	}
	transportCtx.ReleasePluginScope()

	preCtx := root.WithPluginScope(&name)
	if err := p.PreRequestHook(preCtx, req); err != nil {
		t.Fatalf("PreRequestHook() = %v", err)
	}
	headers, _ := preCtx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	got, _ := strconv.ParseInt(headers[p.Header()], 10, 64)

	// It must land near what serializing would say, not near zero.
	want := p.EstimateTokens(req)
	if got < want/2 {
		t.Errorf("estimate=%d for an unretained body, want ~%d: an unmeasured body "+
			"read as an empty one", got, want)
	}
}

func TestTransportHookFallsBackToContentLength(t *testing.T) {
	t.Parallel()

	// A body over the large-payload threshold is not copied, so Body is empty
	// but Content-Length is known. The request is huge and must not read as
	// small.
	p := New()
	root := schemas.NewBifrostContext(t.Context(), time.Now())
	name := Name
	ctx := root.WithPluginScope(&name)

	httpReq := &schemas.HTTPRequest{
		Path:    "/v1/chat/completions",
		Headers: map[string]string{"content-length": "20000000"},
		Body:    nil,
	}
	if _, err := p.HTTPTransportPreHook(ctx, httpReq); err != nil {
		t.Fatalf("HTTPTransportPreHook() = %v", err)
	}
	size, ok := bodyMeasurement(ctx)
	if !ok {
		t.Fatal("no measurement stashed")
	}
	if size.contentLength != 20_000_000 {
		t.Errorf("contentLength = %d, want the declared 20000000", size.contentLength)
	}
}

func TestTransportHookNeverShortCircuits(t *testing.T) {
	t.Parallel()

	// A measurement plugin must never answer the request itself.
	p := New()
	root := schemas.NewBifrostContext(t.Context(), time.Now())
	name := Name
	ctx := root.WithPluginScope(&name)

	resp, err := p.HTTPTransportPreHook(ctx, &schemas.HTTPRequest{
		Path: "/v1/chat/completions",
		Body: chatBody(t, chatRequest(10)),
	})
	if resp != nil {
		t.Error("hook returned a response; it must only observe")
	}
	if err != nil {
		t.Errorf("hook returned an error on a valid request: %v", err)
	}
}

// TestBodyMeasurementMatchesByteClasses is the differential check: counting the
// raw body must agree byte-for-byte with counting a marshal of the same value,
// on the non-ASCII split that the whole estimator depends on. json escaping
// never touches valid non-ASCII, so this equality must hold exactly.
func TestBodyMeasurementMatchesByteClasses(t *testing.T) {
	t.Parallel()

	body := []byte(`{"messages":[{"role":"user","content":"` +
		strings.Repeat("x", 500) + strings.Repeat("я", 300) + `"}]}`)

	c := countByteClasses(body)
	// 300 Cyrillic runes, two bytes each, land in the two-byte bucket.
	if c.twoByte != 600 {
		t.Errorf("twoByte = %d, want 600 (300 two-byte runes)", c.twoByte)
	}
	if c.nonASCII() != 600 {
		t.Errorf("nonASCII() = %d, want 600", c.nonASCII())
	}
	if c.ascii != int64(len(body))-600 {
		t.Errorf("ascii = %d, want %d", c.ascii, int64(len(body))-600)
	}
	// The buckets must sum to the buffer length, or bytes went missing.
	if c.total() != int64(len(body)) {
		t.Errorf("total() = %d, want %d", c.total(), len(body))
	}
}

// TestByteClassesByWidth pins that CJK and emoji land in the 3- and 4-byte
// buckets, not lumped with 2-byte Cyrillic — the fix for F6-b, where one
// non-ASCII divisor mispriced whichever script it was not fitted to.
func TestByteClassesByWidth(t *testing.T) {
	t.Parallel()

	c := countByteClasses([]byte("x" + "я" + "中" + "😀"))
	if c.ascii != 1 {
		t.Errorf("ascii = %d, want 1", c.ascii)
	}
	if c.twoByte != 2 {
		t.Errorf("twoByte = %d, want 2 (я)", c.twoByte)
	}
	if c.threeByte != 3 {
		t.Errorf("threeByte = %d, want 3 (中)", c.threeByte)
	}
	if c.fourByte != 4 {
		t.Errorf("fourByte = %d, want 4 (😀)", c.fourByte)
	}

	// Same character count, but by byte class an emoji prices higher than an
	// ASCII char, and a CJK char higher than Cyrillic — the whole point.
	emoji := priceText(byteClasses{fourByte: 4})
	cjk := priceText(byteClasses{threeByte: 3})
	cyr := priceText(byteClasses{twoByte: 2})
	if emoji < cjk || cjk < cyr {
		t.Errorf("per-char token cost should rise emoji>=cjk>=cyrillic, got %d/%d/%d", emoji, cjk, cyr)
	}
}
