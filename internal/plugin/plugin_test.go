package plugin

import (
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

func TestInitDefaults(t *testing.T) {
	t.Parallel()

	p := New()
	if err := p.Init(nil); err != nil {
		t.Fatalf("Init(nil) = %v, want nil: a plugin row with no config must load", err)
	}
	if got := p.Threshold(); got != defaultThresholdTokens {
		t.Errorf("Threshold() = %d, want %d", got, defaultThresholdTokens)
	}
	if got := p.Header(); got != defaultHeader {
		t.Errorf("Header() = %q, want %q", got, defaultHeader)
	}
}

func TestInitAppliesConfig(t *testing.T) {
	t.Parallel()

	p := New()
	// Shaped like the raw map the .so loader passes through, numbers included:
	// JSON decodes every number as float64.
	err := p.Init(map[string]any{
		"threshold_tokens": float64(1000),
		"header":           "x-big",
	})
	if err != nil {
		t.Fatalf("Init() = %v, want nil", err)
	}
	if got := p.Threshold(); got != 1000 {
		t.Errorf("Threshold() = %d, want 1000", got)
	}
	if got := p.Header(); got != "x-big" {
		t.Errorf("Header() = %q, want %q", got, "x-big")
	}
}

func TestInitRejectsMalformedConfig(t *testing.T) {
	t.Parallel()

	cases := map[string]any{
		"not an object":        "threshold=1",
		"threshold wrong type": map[string]any{"threshold_tokens": "lots"},
		"threshold zero":       map[string]any{"threshold_tokens": float64(0)},
		"threshold negative":   map[string]any{"threshold_tokens": float64(-1)},
		"header wrong type":    map[string]any{"header": 42},
		"header empty":         map[string]any{"header": ""},
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
	empty := &schemas.BifrostRequest{}
	if got := p.EstimateTokens(empty); got != 0 {
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

func TestPreRequestHookFlagsLargeRequests(t *testing.T) {
	t.Parallel()

	p := New()
	if err := p.Init(map[string]any{"threshold_tokens": float64(1000)}); err != nil {
		t.Fatalf("Init() = %v", err)
	}

	ctx := schemas.NewBifrostContext(t.Context(), time.Now())
	if err := p.PreRequestHook(ctx, chatRequest(5000)); err != nil {
		t.Fatalf("PreRequestHook() = %v, want nil", err)
	}

	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if got := headers[defaultHeader]; got != "1" {
		t.Errorf("%s = %q, want \"1\" for an over-threshold request", defaultHeader, got)
	}
}

func TestPreRequestHookOverwritesClientSuppliedValue(t *testing.T) {
	t.Parallel()

	// The headers map starts as a copy of what the CLIENT sent. If the hook
	// only set its header when absent, any caller could route itself by
	// sending it. A small request must come out flagged "0" regardless.
	p := New()
	ctx := schemas.NewBifrostContext(t.Context(), time.Now())
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{
		defaultHeader: "1",
		"user-agent":  "spoofer/1.0",
	})

	if err := p.PreRequestHook(ctx, chatRequest(10)); err != nil {
		t.Fatalf("PreRequestHook() = %v", err)
	}

	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if got := headers[defaultHeader]; got != "0" {
		t.Errorf("%s = %q, want \"0\": a client-supplied value must not survive", defaultHeader, got)
	}
	if got := headers["user-agent"]; got != "spoofer/1.0" {
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
