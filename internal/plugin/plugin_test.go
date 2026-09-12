package plugin

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// chatRequest builds a chat request of roughly approxTokens ASCII tokens, so
// tests can express sizes in tokens rather than bytes.
func chatRequest(approxTokens int) *schemas.BifrostRequest {
	content := strings.Repeat("x", int(float64(approxTokens)*asciiBytesPerToken))
	return &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{{
				Role:    schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentStr: &content},
			}},
		},
	}
}

// headerValue runs the hook and returns what it published.
func headerValue(t *testing.T, p *Plugin, req *schemas.BifrostRequest, seed map[string]string) string {
	t.Helper()

	ctx := schemas.NewBifrostContext(t.Context(), time.Now())
	if seed != nil {
		ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, seed)
	}
	if err := p.PreRequestHook(ctx, req); err != nil {
		t.Fatalf("PreRequestHook() = %v, want nil", err)
	}

	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return headers[p.Header()]
}

func TestInitDefaults(t *testing.T) {
	t.Parallel()

	p := New()
	if err := p.Init(nil); err != nil {
		t.Fatalf("Init(nil) = %v, want nil: a plugin row with no config must load", err)
	}
	if got := p.Header(); got != defaultHeader {
		t.Errorf("Header() = %q, want %q", got, defaultHeader)
	}
}

func TestInitAppliesConfig(t *testing.T) {
	t.Parallel()

	p := New()
	// Shaped like the raw map the .so loader passes through.
	if err := p.Init(map[string]any{"header": "x-tokens"}); err != nil {
		t.Fatalf("Init() = %v, want nil", err)
	}
	if got := p.Header(); got != "x-tokens" {
		t.Errorf("Header() = %q, want %q", got, "x-tokens")
	}
}

func TestInitRejectsMalformedConfig(t *testing.T) {
	t.Parallel()

	cases := map[string]any{
		"not an object":     "header=x-tokens",
		"header wrong type": map[string]any{"header": 42},
		"header empty":      map[string]any{"header": ""},
	}

	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := New().Init(config); err == nil {
				t.Errorf("Init(%#v) = nil, want an error", config)
			}
		})
	}
}

func TestEstimateTokensIgnoresNonContextRequests(t *testing.T) {
	t.Parallel()

	p := New()
	if got := p.EstimateTokens(nil); got != 0 {
		t.Errorf("EstimateTokens(nil) = %d, want 0", got)
	}
	// An embedding request is not context-window bound.
	if got := p.EstimateTokens(&schemas.BifrostRequest{}); got != 0 {
		t.Errorf("EstimateTokens(no payload) = %d, want 0", got)
	}
}

func TestEstimateTokensScalesWithPayload(t *testing.T) {
	t.Parallel()

	p := New()
	small := p.EstimateTokens(chatRequest(100))
	large := p.EstimateTokens(chatRequest(10_000))

	if small >= large {
		t.Errorf("estimate did not grow with payload: small=%d large=%d", small, large)
	}
	// The estimate is coarse by design; assert the order of magnitude only.
	if large < 5_000 || large > 20_000 {
		t.Errorf("EstimateTokens(~10k tokens) = %d, want roughly 10k", large)
	}
}

func TestEstimateTokensCountsToolDefinitions(t *testing.T) {
	t.Parallel()

	// An agent client resends its whole toolset every turn, so a big toolset
	// with a one-line message is exactly the request that must not look small.
	description := strings.Repeat("d", 400_000)
	p := New()
	req := chatRequest(1)
	req.ChatRequest.Params = &schemas.ChatParameters{
		Tools: []schemas.ChatTool{{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        "search",
				Description: &description,
			},
		}},
	}

	if got := p.EstimateTokens(req); got < 50_000 {
		t.Errorf("EstimateTokens(400kB of tool definitions) = %d, want the tools to count", got)
	}
}

// inlineImageRequest builds a chat request carrying one base64 data: URL of the
// given decoded size, the shape a client uses to attach a screenshot.
func inlineImageRequest(decodedBytes int) *schemas.BifrostRequest {
	url := "data:image/png;base64," + strings.Repeat("A", decodedBytes*4/3)
	return &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
					Type:           schemas.ChatContentBlockTypeImage,
					ImageURLStruct: &schemas.ChatInputImage{URL: url},
				}}},
			}},
		},
	}
}

func TestEstimateTokensPricesImagesByModalityNotBytes(t *testing.T) {
	t.Parallel()

	// A 1 MB screenshot is ~1.4 MB of base64 in the body. Priced by bytes it
	// would score ~350k tokens and route a trivial request to a long-context
	// provider; providers bill it at 85-1568 depending on pixel size.
	p := New()
	got := p.EstimateTokens(inlineImageRequest(1 << 20))

	if got > 4*imageTokens {
		t.Errorf("EstimateTokens(1MB inline image) = %d, want on the order of %d", got, imageTokens)
	}
	if got < imageTokens {
		t.Errorf("EstimateTokens(1MB inline image) = %d, want at least the per-image cost %d", got, imageTokens)
	}
}

func TestEstimateTokensChargesRemoteImagesToo(t *testing.T) {
	t.Parallel()

	// The opposite error: an https:// URL is ~80 bytes in the body but the
	// provider downloads the picture and bills the same ~1k tokens.
	p := New()
	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
					Type:           schemas.ChatContentBlockTypeImage,
					ImageURLStruct: &schemas.ChatInputImage{URL: "https://example.com/screenshot.png"},
				}}},
			}},
		},
	}

	if got := p.EstimateTokens(req); got < imageTokens {
		t.Errorf("EstimateTokens(remote image) = %d, want at least %d", got, imageTokens)
	}
}

func TestEstimateTokensCountsAudioAndFiles(t *testing.T) {
	t.Parallel()

	p := New()
	fileData := strings.Repeat("B", 1<<20)
	audio := strings.Repeat("C", 1<<20)

	audioReq := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
					Type:       schemas.ChatContentBlockTypeInputAudio,
					InputAudio: &schemas.ChatInputAudio{Data: audio},
				}}},
			}},
		},
	}
	fileReq := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{{
				Role: schemas.ChatMessageRoleUser,
				Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{{
					Type: schemas.ChatContentBlockTypeFile,
					File: &schemas.ChatInputFile{FileData: &fileData},
				}}},
			}},
		},
	}

	// Both must be far below what the same bytes would cost as text, and far
	// above zero: they consume context, just not one token per few base64
	// characters.
	mediaBytes := int64(1 << 20)
	textEstimate := int64(float64(mediaBytes) / asciiBytesPerToken)
	for name, req := range map[string]*schemas.BifrostRequest{"audio": audioReq, "file": fileReq} {
		got := p.EstimateTokens(req)
		if got >= textEstimate {
			t.Errorf("EstimateTokens(1MB %s) = %d, want well below the byte estimate %d", name, got, textEstimate)
		}
		if got == 0 {
			t.Errorf("EstimateTokens(1MB %s) = 0, want a non-zero cost", name)
		}
	}
}

func TestEstimateTokensStillCountsTextAroundMedia(t *testing.T) {
	t.Parallel()

	// Subtracting the media bytes must not swallow the text next to them.
	p := New()
	text := strings.Repeat("x", 400_000)
	req := inlineImageRequest(1 << 20)
	req.ChatRequest.Input = append(req.ChatRequest.Input, schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentStr: &text},
	})

	if got := p.EstimateTokens(req); got < 50_000 {
		t.Errorf("EstimateTokens(image + 400kB text) = %d, want the text to dominate", got)
	}
}

func TestPreRequestHookPublishesTheCount(t *testing.T) {
	t.Parallel()

	// The header carries the number itself, so a rule can pick its own
	// threshold with int(headers[...]) rather than trusting one baked in here.
	p := New()
	req := chatRequest(10_000)

	got := headerValue(t, p, req, nil)
	parsed, err := strconv.ParseInt(got, 10, 64)
	if err != nil {
		t.Fatalf("header %q is not an integer: %v", got, err)
	}
	if parsed != p.EstimateTokens(req) {
		t.Errorf("header = %d, want %d (the estimate)", parsed, p.EstimateTokens(req))
	}
	if parsed < 5_000 || parsed > 20_000 {
		t.Errorf("header = %d, want roughly 10k", parsed)
	}
}

func TestPreRequestHookReportsZeroForSmallRequests(t *testing.T) {
	t.Parallel()

	// Not "absent": a rule comparing int(headers[...]) needs a value on every
	// request, and a missing header would make int() fail rather than compare.
	if got := headerValue(t, New(), &schemas.BifrostRequest{}, nil); got != "0" {
		t.Errorf("header = %q, want \"0\" for a request with no payload", got)
	}
}

func TestPreRequestHookOverwritesClientSuppliedValue(t *testing.T) {
	t.Parallel()

	// The headers map starts as a copy of what the CLIENT sent. If the hook
	// did not overwrite, a caller could claim any size and route itself.
	p := New()
	seed := map[string]string{
		defaultHeader: "999999999",
		"user-agent":  "spoofer/1.0",
	}

	if got := headerValue(t, p, chatRequest(10), seed); got == "999999999" {
		t.Error("a client-supplied count survived the hook")
	}
}

func TestPreRequestHookPreservesOtherHeaders(t *testing.T) {
	t.Parallel()

	p := New()
	ctx := schemas.NewBifrostContext(t.Context(), time.Now())
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{"user-agent": "curl/8"})

	if err := p.PreRequestHook(ctx, chatRequest(10)); err != nil {
		t.Fatalf("PreRequestHook() = %v", err)
	}

	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if got := headers["user-agent"]; got != "curl/8" {
		t.Errorf("user-agent = %q, want the original value preserved", got)
	}
}

func TestPreRequestHookDoesNotMutateTheExistingMap(t *testing.T) {
	t.Parallel()

	// The map is shared with other hooks and read concurrently, so the hook
	// must publish a new one rather than write into the old.
	p := New()
	original := map[string]string{"user-agent": "curl/8"}
	ctx := schemas.NewBifrostContext(t.Context(), time.Now())
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, original)

	if err := p.PreRequestHook(ctx, chatRequest(10)); err != nil {
		t.Fatalf("PreRequestHook() = %v", err)
	}

	if _, present := original[defaultHeader]; present {
		t.Error("the pre-existing map was mutated in place")
	}
}

func TestPreRequestHookToleratesNilContext(t *testing.T) {
	t.Parallel()

	if err := New().PreRequestHook(nil, nil); err != nil {
		t.Errorf("PreRequestHook(nil, nil) = %v, want nil", err)
	}
}

func TestCleanupIsSafe(t *testing.T) {
	t.Parallel()

	if err := New().Cleanup(); err != nil {
		t.Errorf("Cleanup() = %v, want nil", err)
	}
}
