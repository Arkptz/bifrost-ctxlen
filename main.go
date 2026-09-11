// Command bifrost-plugin is a Bifrost plugin, loaded at runtime via plugin.Open.
//
// This file is only a shim. It must be `package main` with NO func main,
// because `go build -buildmode=plugin` compiles a DIRECTORY into one .so and
// the loader resolves plain package-level symbols out of it. All behaviour
// lives in internal/plugin so the same code can also be compiled into a
// bifrost-http binary instead of being dlopen'd.
//
// The loader (framework/plugins/soloader.go) requires exactly two symbols,
// GetName and Cleanup, and looks the rest up OPTIONALLY: a hook whose name or
// signature is wrong is silently skipped, and the plugin loads looking healthy
// while doing nothing. hooks_assert.go exists to turn that runtime silence into
// a compile error — extend it whenever you export a new hook.
package main

import (
	"github.com/Arkptz/bifrost-ctxlen/internal/plugin"

	"github.com/maximhq/bifrost/core/schemas"
)

// instance is the single plugin object shared by every exported hook. The
// loader has nowhere to hand one to us, so package state is the only option.
var instance = plugin.New()

// GetName returns the plugin's system identifier. REQUIRED by the loader.
func GetName() string { return plugin.Name }

// Init receives the plugin's stored config as a raw map. OPTIONAL.
func Init(config any) error { return instance.Init(config) }

// PreRequestHook runs before provider selection, and before the built-in
// governance and routing plugins when this plugin is registered with
// "placement": "pre_builtin". OPTIONAL.
func PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	return instance.PreRequestHook(ctx, req)
}

// Cleanup runs at shutdown. REQUIRED by the loader.
func Cleanup() error { return instance.Cleanup() }
