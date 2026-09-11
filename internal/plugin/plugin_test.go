package plugin

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// chatRequest builds a chat request whose serialized payload is at least
// approxTokens*bytesPerToken bytes, so tests can express sizes in tokens.
func chatRequest(approxTokens int) *schemas.BifrostRequest {
	content := strings.Repeat("x", approxTokens*bytesPerToken)
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
