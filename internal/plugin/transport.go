package plugin

import (
	"strconv"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

// bodySizeKey carries the raw-body measurement from the transport hook to
// PreRequestHook.
//
// A BifrostContextKey and not a plain string: the transport middleware sweeps
// every STRING-keyed user value into HTTPRequest.PathParams, so a string key
// would quietly become a path parameter. The value is a struct pointer for the
// same reason — it can never be mistaken for one.
const bodySizeKey schemas.BifrostContextKey = "ctxlen-body-size"

// bodySize is the raw request body, counted by byte class.
//
// Stored rather than the body itself because HTTPRequest is pooled and
// ReleaseHTTPRequest nils Body as soon as the hook returns — holding the slice
// would alias memory the next request is about to reuse.
type bodySize struct {
	ascii    int64
	nonASCII int64
	// contentLength is the client's declared size, used when the body itself
	// was not retained. Negative when unknown.
	contentLength int64
}

// inferencePaths are the request paths worth measuring.
//
// The transport hook fires on every route the middleware chain covers,
// including the admin API. Scanning the body of a config write would cost real
// time and tell us nothing.
func isInferencePath(path string) bool {
	return strings.Contains(path, "/chat/completions") ||
		strings.Contains(path, "/responses") ||
		strings.Contains(path, "/messages") ||
		strings.Contains(path, "/generateContent")
}

// HTTPTransportPreHook measures the raw request body before Bifrost parses it.
//
// This exists to remove a json.Marshal from the hot path. Measuring the parsed
// structs means re-serializing them — 76% of this plugin's cost and nearly all
// of its allocations — while the bytes that serialization reproduces were
// already on the wire. The transport layer has them: fasthttpToHTTPRequest
// copies the (decompressed) body into HTTPRequest.Body, and it does so on every
// request ALREADY, because a loaded .so registers as an HTTP transport plugin
// whether or not it exports this hook. So the copy is paid for today and thrown
// away; this reads it.
//
// It measures and stashes only. The estimate is still published by
// PreRequestHook, which is where routing can see it.
func (p *Plugin) HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	if ctx == nil || req == nil || !isInferencePath(req.Path) {
		return nil, nil
	}

	size := bodySize{contentLength: -1}
	if raw := req.CaseInsensitiveHeaderLookup("content-length"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			size.contentLength = n
		}
	}

	// An empty body is not an error: the transport skips the copy for payloads
	// over the large-payload threshold (10 MB by default) and for chunked
	// requests whose length is unknown. Content-Length is then the only signal,
	// and a request that large is above any sane routing threshold anyway.
	if len(req.Body) > 0 {
		size.ascii, size.nonASCII = countByteClasses(req.Body)
	}

	ctx.SetValue(bodySizeKey, &size)

	// Never short-circuit. This hook observes; returning a response here would
	// answer the request from a plugin whose job is to measure it.
	return nil, nil
}

// bodyMeasurement returns what the transport hook measured, if it ran.
func bodyMeasurement(ctx *schemas.BifrostContext) (bodySize, bool) {
	if ctx == nil {
		return bodySize{}, false
	}
	size, ok := ctx.Value(bodySizeKey).(*bodySize)
	if !ok || size == nil {
		return bodySize{}, false
	}
	return *size, true
}
