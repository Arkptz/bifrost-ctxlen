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

	headers := runWithTransport(t, p, req, body, "/v1/chat/completions")

	bodyEst, err := strconv.ParseInt(headers[headerPrefix+"bodyest"], 10, 64)
	if err != nil {
		t.Fatalf("bodyest = %q, not an integer: %v", headers[headerPrefix+"bodyest"], err)
	}
	if bodyEst == 0 {
		t.Fatal("bodyest = 0: the transport hook did not measure the body")
	}

	// The body path and the marshal path measure slightly different things
	// (the body carries top-level params, the marshal does not), but for a
	// text-only request they must land within a few percent — that closeness is
	// the whole premise of replacing one with the other.
	marshalEst, _ := strconv.ParseInt(headers[p.Header()], 10, 64)
	ratio := float64(bodyEst) / float64(marshalEst)
	if ratio < 0.9 || ratio > 1.15 {
		t.Errorf("bodyest=%d marshalest=%d (ratio %.3f): the two paths disagree by more than the shadow window",
			bodyEst, marshalEst, ratio)
	}
}

// TestBodyEstimateSubtractsInlineMedia is the regression for a real defect: the
// raw body carries inline base64 in full, and counting it as text priced a 1 MB
// screenshot as ~350k tokens. The body estimate must subtract media the same way
// the marshal estimate does.
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
	bodyEst, _ := strconv.ParseInt(headers[headerPrefix+"bodyest"], 10, 64)

	// The image is ~1 MB of base64 ASCII. Priced as text it would be hundreds
	// of thousands of tokens; priced by modality it is ~1.6k plus the tiny text.
	if bodyEst > 4*imageTokens {
		t.Errorf("bodyest=%d for a 1 MB inline image: media bytes were counted as text",
			bodyEst)
	}
	// And it must agree with the marshal path, which already subtracts media.
	marshalEst := p.EstimateTokens(req)
	ratio := float64(bodyEst) / float64(max(marshalEst, 1))
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("bodyest=%d marshalest=%d: the two media accountings disagree", bodyEst, marshalEst)
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

	ascii, nonASCII := countByteClasses(body)
	// 300 Cyrillic runes, two bytes each.
	if nonASCII != 600 {
		t.Errorf("nonASCII = %d, want 600 (300 two-byte runes)", nonASCII)
	}
	if ascii != int64(len(body))-600 {
		t.Errorf("ascii = %d, want %d", ascii, int64(len(body))-600)
	}
}
