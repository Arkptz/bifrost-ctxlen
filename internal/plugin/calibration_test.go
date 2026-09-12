package plugin

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

// The accuracy this estimator is held to, against what providers actually
// billed. These are deliberately tight: the point of the fixture is to fail
// when a constant is changed without re-calibrating, which is exactly how the
// previous 30-39% underestimate survived a full test suite.
const (
	// maxOverestimate is generous on purpose. Overestimating routes a request
	// to a long-context provider that could have gone to the cheap one — it
	// costs money, not correctness.
	maxOverestimate = 0.15

	// maxUnderestimate is tight on purpose, and asymmetric. Underestimating
	// sends a genuinely large request to a short-context provider, which fails
	// the request mid-session. This is the direction that hurts.
	maxUnderestimate = 0.10
)

// calibrationCase is one real production request, reduced to the shape the
// estimator actually consumes.
//
// The byte counts are derived from live traffic captured off the gateway's own
// /api/logs, paired with the prompt_tokens the provider billed for it. The
// original transcripts are NOT in this repository: they are session logs full
// of file paths and infrastructure detail, and this repo is public. Only the
// profile survives — byte classes, message and tool counts, billed tokens —
// which is all the estimator's arithmetic depends on.
type calibrationCase struct {
	Messages           int   `json:"messages"`
	Tools              int   `json:"tools"`
	MessageASCIIBytes  int   `json:"message_ascii_bytes"`
	MessageNonASCII    int   `json:"message_nonascii_bytes"`
	ToolASCIIBytes     int   `json:"tool_ascii_bytes"`
	ToolNonASCII       int   `json:"tool_nonascii_bytes"`
	BilledPromptTokens int64 `json:"billed_prompt_tokens"`
	CachedTokens       int64 `json:"cached_tokens"`
}

// synthesize rebuilds a request with the same byte profile as the original.
//
// Content is filler, because the estimator never looks at what the bytes SAY —
// only at how many there are of each class. Cyrillic is the non-ASCII filler
// because that is what the real traffic contained, and at two bytes per
// character it reproduces the original ratio exactly.
func (c calibrationCase) synthesize(t *testing.T) *schemas.BifrostRequest {
	t.Helper()

	// Each message costs a few bytes of JSON framing (braces, the role, the
	// content key). Measure it once and subtract, so the synthetic payload
	// lands on the recorded byte count rather than overshooting by framing.
	probe, err := json.Marshal([]schemas.ChatMessage{newTextMessage("")})
	if err != nil {
		t.Fatalf("probe marshal: %v", err)
	}
	framing := len(probe) - len("[]")

	msgs := max(c.Messages, 1)
	asciiPerMsg := max(c.MessageASCIIBytes/msgs-framing, 0)
	// Cyrillic filler is two bytes per rune, so half as many runes as bytes.
	nonASCIIRunes := c.MessageNonASCII / 2 / msgs

	input := make([]schemas.ChatMessage, 0, msgs)
	for range msgs {
		input = append(input, newTextMessage(
			strings.Repeat("x", asciiPerMsg)+strings.Repeat("я", nonASCIIRunes),
		))
	}

	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{Input: input},
	}

	if c.Tools > 0 {
		tools := max(c.Tools, 1)
		descASCII := max(c.ToolASCIIBytes/tools, 0)
		descRunes := c.ToolNonASCII / 2 / tools
		defs := make([]schemas.ChatTool, 0, tools)
		for range tools {
			description := strings.Repeat("d", descASCII) + strings.Repeat("я", descRunes)
			defs = append(defs, schemas.ChatTool{
				Type: schemas.ChatToolTypeFunction,
				Function: &schemas.ChatToolFunction{
					Name:        "tool",
					Description: &description,
				},
			})
		}
		req.ChatRequest.Params = &schemas.ChatParameters{Tools: defs}
	}

	return req
}

func newTextMessage(content string) schemas.ChatMessage {
	return schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentStr: &content},
	}
}

func loadCalibration(t *testing.T) []calibrationCase {
	t.Helper()

	raw, err := os.ReadFile("testdata/calibration.json")
	if err != nil {
		t.Fatalf("read calibration fixture: %v", err)
	}
	var cases []calibrationCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse calibration fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("calibration fixture is empty")
	}
	return cases
}

// TestEstimateMatchesBilledTokens is the test that would have caught the
// original defect: every unit test passed while the estimator was 39% low,
// because they all asserted against bands derived from the estimator itself.
// This one asserts against an external oracle — what the provider charged.
func TestEstimateMatchesBilledTokens(t *testing.T) {
	t.Parallel()

	p := New()
	for _, c := range loadCalibration(t) {
		got := p.EstimateTokens(c.synthesize(t))
		err := (float64(got) - float64(c.BilledPromptTokens)) / float64(c.BilledPromptTokens)

		switch {
		case err > maxOverestimate:
			t.Errorf("billed=%d estimate=%d: %.1f%% over, limit %.0f%%",
				c.BilledPromptTokens, got, 100*err, 100*maxOverestimate)
		case -err > maxUnderestimate:
			t.Errorf("billed=%d estimate=%d: %.1f%% UNDER, limit %.0f%% — an underestimate routes a large request to a short-context provider",
				c.BilledPromptTokens, got, -100*err, 100*maxUnderestimate)
		}
	}
}

// TestCyrillicCostsMoreThanASCII pins the defect's root cause directly.
//
// Go serializes non-ASCII into JSON as raw UTF-8, so the same VISIBLE text is
// twice the bytes in Cyrillic. A single bytes-per-token divisor therefore
// misprices one of the two scripts no matter what it is set to. Pin the
// relationship rather than the numbers, so it survives re-calibration.
func TestCyrillicCostsMoreThanASCII(t *testing.T) {
	t.Parallel()

	p := New()
	const runes = 100_000

	ascii := strings.Repeat("x", runes)
	cyrillic := strings.Repeat("я", runes)

	asciiEst := p.EstimateTokens(&schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{newTextMessage(ascii)},
		},
	})
	cyrillicEst := p.EstimateTokens(&schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{newTextMessage(cyrillic)},
		},
	})

	// Same character count, so the estimates must be comparable — not the 2x
	// apart that counting raw bytes with one divisor would produce.
	ratio := float64(cyrillicEst) / float64(asciiEst)
	if ratio < 1.0 {
		t.Errorf("cyrillic=%d ascii=%d (ratio %.2f): Cyrillic must not cost LESS per character than ASCII",
			cyrillicEst, asciiEst, ratio)
	}
	if ratio > 1.6 {
		t.Errorf("cyrillic=%d ascii=%d (ratio %.2f): Cyrillic is being charged per BYTE rather than per character",
			cyrillicEst, asciiEst, ratio)
	}
}

// TestThresholdDecisionsHoldOnRealTraffic asserts the behaviour that actually
// matters: which side of a routing threshold each real request lands on. An
// estimator can have an acceptable average error and still misroute at the
// boundary, which is the only thing a routing rule ever asks it.
func TestThresholdDecisionsHoldOnRealTraffic(t *testing.T) {
	t.Parallel()

	p := New()
	const threshold = 500_000

	for _, c := range loadCalibration(t) {
		got := p.EstimateTokens(c.synthesize(t))
		wantLarge := c.BilledPromptTokens > threshold
		gotLarge := got > threshold

		if wantLarge != gotLarge {
			t.Errorf("billed=%d estimate=%d: routed as large=%v, should be %v at a %d threshold",
				c.BilledPromptTokens, got, gotLarge, wantLarge, threshold)
		}
	}
}
