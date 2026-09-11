package main

import "github.com/maximhq/bifrost/core/schemas"

// Compile-time proof that every exported hook has the exact name and signature
// plugin.Open looks up.
//
// This matters because the loader uses OPTIONAL symbol lookup. Misspell
// `PreRequestHook`, or drift one parameter type, and the symbol simply is not
// found: the plugin loads, reports healthy, and silently never runs that hook.
// Binding each symbol to its expected function type here turns both mistakes
// into a build failure instead — and, unlike a _test.go file, this is part of
// the package, so `go build -buildmode=plugin` fails too, not just `go test`.
//
// Add a line here for every hook you export. The signatures come from
// framework/plugins/soloader.go, which is the only authority on them.
var (
	_ func() string    = GetName
	_ func() error     = Cleanup
	_ func(any) error  = Init
	_ preRequestHookFn = PreRequestHook
)

type preRequestHookFn = func(*schemas.BifrostContext, *schemas.BifrostRequest) error
