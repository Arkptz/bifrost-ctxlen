// Command loader dlopens a built plugin and resolves every symbol bifrost
// would resolve, so a broken .so fails in CI instead of on the gateway.
//
// This catches what a unit test cannot. `go test` exercises the hook functions
// in-process, which proves the logic but says nothing about whether the .so
// LOADS: Go's plugin runtime compares a hash of every package shared between
// host and plugin, and refuses with "plugin was built with a different version
// of package X" when they differ. That hash covers the whole transitive
// closure, so an unrelated indirect dependency moving is enough to break it.
//
// The import of core/schemas below is therefore load-bearing, not decoration.
// It makes schemas a SHARED package between this harness and the plugin, which
// is what arms the version check. Drop the import and the harness still builds,
// still "passes", and verifies nothing.
//
// Caveat worth knowing: passing here proves the plugin agrees with the core
// version in THIS module's go.mod. It does not prove it agrees with a patched,
// self-built bifrost-http — for that the harness has to be built from the
// host's own source tree.
//
// Usage: loader <path to plugin.so>
package main

import (
	"fmt"
	"os"
	"plugin"

	"github.com/maximhq/bifrost/core/schemas"
)

// required mirrors framework/plugins/soloader.go: GetName and Cleanup must
// exist, every other hook is optional. Keep this list in sync with the symbols
// the plugin actually exports.
var required = []string{"GetName", "Cleanup"}

// optional hooks are reported but not enforced, so the harness stays useful for
// a plugin that implements a different subset.
var optional = []string{"Init", "PreRequestHook"}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <plugin.so>\n", os.Args[0])
		os.Exit(2)
	}
	path := os.Args[1]

	// This is the call that enforces the ABI match; a mismatch surfaces here.
	p, err := plugin.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: plugin.Open(%s): %v\n", path, err)
		os.Exit(1)
	}

	failed := false
	for _, name := range required {
		if _, err := p.Lookup(name); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: required symbol %s missing: %v\n", name, err)
			failed = true
			continue
		}
		fmt.Printf("ok       %s\n", name)
	}

	for _, name := range optional {
		if _, err := p.Lookup(name); err != nil {
			fmt.Printf("absent   %s (optional)\n", name)
			continue
		}
		fmt.Printf("ok       %s\n", name)
	}

	// Calling GetName proves the symbol is not merely present but callable
	// with the signature the loader will cast it to.
	sym, err := p.Lookup("GetName")
	if err == nil {
		getName, ok := sym.(func() string)
		if !ok {
			fmt.Fprintf(os.Stderr, "FAIL: GetName has type %T, want func() string\n", sym)
			failed = true
		} else {
			fmt.Printf("name     %s\n", getName())
		}
	}

	if failed {
		os.Exit(1)
	}

	// Reference schemas so the import cannot be dropped as unused: see the
	// package comment for why that import is what makes this check real.
	var _ schemas.BifrostContext
	fmt.Println("PASS: plugin loaded and all required symbols resolved")
}
