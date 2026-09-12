package plugin

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// runHook runs the hook on a properly scoped context and returns the published
// headers plus whatever was logged.
func runHook(t *testing.T, p *Plugin, req *schemas.BifrostRequest) (map[string]string, []schemas.PluginLogEntry) {
	t.Helper()

	root, scoped := scopedContext(t)
	if err := p.PreRequestHook(scoped, req); err != nil {
		t.Fatalf("PreRequestHook() = %v, want nil", err)
	}

	headers, _ := scoped.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	return headers, root.GetPluginLogs()
}

// TestPreRequestHookPublishesTheBreakdownAsHeaders pins the headers a consumer
// can read. The total keeps its own name because routing rules compare it.
func TestPreRequestHookPublishesTheBreakdownAsHeaders(t *testing.T) {
	t.Parallel()

	p := New()
	headers, _ := runHook(t, p, chatRequest(10_000))

	if _, present := headers[p.Header()]; !present {
		t.Fatalf("the total is missing from %v", headers)
	}

	// Every breakdown field must be present and parse as an integer: a rule may
	// compare any of them with int(), and a missing key makes CEL treat the
	// whole expression as a non-match rather than fail.
	for _, name := range []string{
		"msgs", "tools", "sys", "ascii", "nonascii", "mediab",
		"text", "frame", "media", "img", "audio", "doc", "docurl",
	} {
		key := headerPrefix + name
		got, present := headers[key]
		if !present {
			t.Errorf("%s is missing", key)
			continue
		}
		if _, err := strconv.ParseInt(got, 10, 64); err != nil {
			t.Errorf("%s = %q, want an integer: %v", key, got, err)
		}
	}

	if got := headers[headerPrefix+"kind"]; got != kindChat {
		t.Errorf("%skind = %q, want %q", headerPrefix, got, kindChat)
	}
}

// TestPublishedBreakdownAddsUp checks the invariant a reader is invited to
// verify by eye, and which makes the published numbers self-checking.
func TestPublishedBreakdownAddsUp(t *testing.T) {
	t.Parallel()

	p := New()
	estimate := p.Estimate(chatRequest(10_000))

	if sum := estimate.Text + estimate.Framing + estimate.Media; sum != estimate.Total {
		t.Errorf("Total = %d, but Text+Framing+Media = %d", estimate.Total, sum)
	}
	if want := estimate.Messages * tokensPerMessage; estimate.Framing != want {
		t.Errorf("Framing = %d, want messages*%d = %d", estimate.Framing, tokensPerMessage, want)
	}
}

// TestLogLineIsParseable pins the log format. The message is an unstructured
// string, so its shape IS the interface: someone will grep it, and the drift
// query in the README parses est= out of it with a regex.
func TestLogLineIsParseable(t *testing.T) {
	t.Parallel()

	p := New()
	_, logs := runHook(t, p, chatRequest(10_000))

	if len(logs) != 1 {
		t.Fatalf("got %d log entries, want exactly 1 per request", len(logs))
	}
	entry := logs[0]
	if entry.PluginName != Name {
		t.Errorf("PluginName = %q, want %q", entry.PluginName, Name)
	}
	if entry.Level != schemas.LogLevelInfo {
		t.Errorf("Level = %q, want info: a routine measurement is not a warning", entry.Level)
	}

	fields := strings.Fields(entry.Message)
	if fields[0] != logSchema {
		t.Fatalf("first token = %q, want the schema anchor %q", fields[0], logSchema)
	}
	if !strings.HasPrefix(fields[1], "est=") {
		t.Fatalf("second token = %q, want est=...", fields[1])
	}

	// Every token after the anchor must be key=value with no spaces or quotes,
	// which is what makes the line safe to parse and impossible to forge with
	// request content.
	for _, token := range fields[1:] {
		key, value, ok := strings.Cut(token, "=")
		if !ok || key == "" || value == "" {
			t.Errorf("token %q is not key=value", token)
			continue
		}
		if key == "kind" {
			continue
		}
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			t.Errorf("token %q: value is not an integer, so the line is not machine-readable", token)
		}
	}
}

// TestPublishedValuesNeverCarryRequestContent is the leak guard.
//
// Everything published is an integer or a kind from a closed set, so no message
// text, tool name or URL can reach a header or a log line. That matters twice
// over: these land in a Postgres table humans query, and a tool named
// `x est=1` would otherwise forge a field in the drift query.
func TestPublishedValuesNeverCarryRequestContent(t *testing.T) {
	t.Parallel()

	const secret = "SUPERSECRET"
	description := secret + " tool description"
	content := secret + " message body"

	req := &schemas.BifrostRequest{
		ChatRequest: &schemas.BifrostChatRequest{
			Input: []schemas.ChatMessage{newTextMessage(content)},
			Params: &schemas.ChatParameters{
				Tools: []schemas.ChatTool{{
					Type: schemas.ChatToolTypeFunction,
					Function: &schemas.ChatToolFunction{
						Name:        secret + "_tool",
						Description: &description,
					},
				}},
			},
		},
	}

	p := New()
	headers, logs := runHook(t, p, req)

	for key, value := range headers {
		if !strings.HasPrefix(key, headerPrefix) && key != p.Header() {
			continue // not ours
		}
		if strings.Contains(value, secret) {
			t.Errorf("header %s leaked request content: %q", key, value)
		}
	}
	for _, entry := range logs {
		if strings.Contains(entry.Message, secret) {
			t.Errorf("log line leaked request content: %q", entry.Message)
		}
	}
}

// TestUnmeasurablePayloadIsReported covers the case that used to be silent.
//
// A payload that will not serialize estimates to zero, which makes a large
// request look small — the exact misroute this plugin exists to prevent. It has
// to be visible, and distinguishable by a parser before it looks for est=.
//
// The state is constructed rather than provoked: every field the estimator
// serializes is a concrete type that always marshals, so reaching the error
// branch through a real request would mean asserting on a bug elsewhere.
func TestUnmeasurablePayloadIsReported(t *testing.T) {
	t.Parallel()

	line := Estimate{Kind: kindChat, Unmeasurable: true}.logLine()

	if !strings.HasPrefix(line, logSchema+" warn=payload_unmarshalable") {
		t.Errorf("log line = %q, want the schema anchor then a warn= token", line)
	}
	if strings.Contains(line, "est=") {
		t.Errorf("log line = %q: it must not carry an est= a parser would believe", line)
	}
}

// TestHookFailsLoudlyWhenLoggingIsNotRecorded is the regression test for the
// defect class this instrumentation exists to prevent.
//
// ctx.Log is a silent no-op on a context the host did not scope to the plugin.
// Code that looks instrumented and emits nothing is worse than no
// instrumentation, so the hook must report it — through the error return, the
// one channel that still works when logging does not.
//
// This cannot catch the host itself changing: it reproduces the host's call
// rather than making it. The runtime check is what covers that.
func TestHookFailsLoudlyWhenLoggingIsNotRecorded(t *testing.T) {
	t.Parallel()

	// Deliberately UNSCOPED, the way a host that dropped WithPluginScope would
	// call us.
	ctx := schemas.NewBifrostContext(t.Context(), time.Now())

	p := New()
	err := p.PreRequestHook(ctx, chatRequest(10))
	if err == nil {
		t.Fatal("PreRequestHook() = nil on an unscoped context: the estimate is " +
			"unobservable and nothing said so")
	}
	if !strings.Contains(err.Error(), "scoping") {
		t.Errorf("error = %q, want it to name the cause", err)
	}

	// The measurement must still have happened: the error is non-blocking, so
	// routing has to keep working even when observability does not.
	headers, _ := ctx.Value(schemas.BifrostContextKeyRequestHeaders).(map[string]string)
	if _, present := headers[p.Header()]; !present {
		t.Error("the header was not published; a logging failure must not stop the routing input")
	}
}

// TestLoggingIsVerifiedOnlyOnce pins the cost decision: GetPluginLogs
// deep-copies, so the check runs at startup and never again.
func TestLoggingIsVerifiedOnlyOnce(t *testing.T) {
	t.Parallel()

	p := New()
	unscoped := schemas.NewBifrostContext(t.Context(), time.Now())

	if err := p.PreRequestHook(unscoped, chatRequest(10)); err == nil {
		t.Fatal("first call should have reported the unscoped context")
	}
	if err := p.PreRequestHook(unscoped, chatRequest(10)); err != nil {
		t.Errorf("second call = %v, want nil: the check is once per process", err)
	}
}
