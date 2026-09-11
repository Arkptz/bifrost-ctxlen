{
  description = "Bifrost gateway plugin (Go -buildmode=plugin) with quality gates + dg decision graph";

  inputs = {
    # nixos-unstable, not the stable channel the sibling templates track: this
    # needs a `go_1_27` attribute to override, and the stable channel stops at
    # go_1_26. Bifrost's modules declare `go 1.27.0`, so an older toolchain
    # cannot build against them at all.
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
  };

  outputs = inputs @ {flake-parts, ...}:
    flake-parts.lib.mkFlake {inherit inputs;} {
      # Go plugins are dlopen'd, which Windows does not support and which needs
      # the plugin and host to share a platform. Linux and Darwin only.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
        "x86_64-darwin"
      ];

      perSystem = {system, ...}: let
        # nixpkgs ships 1.27 as a release candidate, and Go orders "1.27rc2"
        # BEFORE "1.27.0", so every module declaring `go 1.27.0` — which
        # bifrost's do — is rejected with "requires go >= 1.27.0 (running
        # go1.27rc2)". A module pulled from the module cache cannot be
        # rewritten, so unlike a monorepo build there is nothing to patch: the
        # toolchain itself has to be the real 1.27.0.
        #
        # Same overlay upstream uses in its own flake.nix. Delete it once
        # nixpkgs ships a final 1.27.x — `go_1_27` then already satisfies the
        # constraint and this only costs a source build.
        go_1_27_0_overlay = final: prev: {
          go_1_27 = prev.go_1_27.overrideAttrs (_: rec {
            version = "1.27.0";
            src = final.fetchurl {
              url = "https://go.dev/dl/go${version}.src.tar.gz";
              sha256 = "7002403d7cc44529ef6d26f69a44818263395ead7c16c05a5808ae047ebeb0e5";
            };
          });
          go = final.go_1_27;
        };

        pkgs = import inputs.nixpkgs {
          inherit system;
          overlays = [go_1_27_0_overlay];
        };

        pname = "bifrost-ctxlen";
        version = "0.1.0";

        # vendorHash covers the whole dependency tree. Refresh it with:
        #   nix build 2>&1 | grep 'got:' | awk '{print $2}'
        vendorHash = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";
      in {
        # Eval-only placeholder — real build/test/lint run via lefthook / CI.
        checks.build-check = pkgs.runCommand "build-check" {} ''
          touch $out
        '';

        # The .so itself.
        #
        # buildGoModule has no -buildmode=plugin support, so the build and
        # install phases are replaced. Everything else is left alone on purpose:
        # configurePhase sets up the vendor tree, and GOFLAGS carries -trimpath,
        # which feeds the package hashes the plugin runtime compares against the
        # host. Override those and the .so stops loading.
        packages.default = pkgs.buildGoModule {
          inherit pname version vendorHash;
          src = ./.;

          # cgo is mandatory for -buildmode=plugin.
          env.CGO_ENABLED = "1";
          nativeBuildInputs = [pkgs.gcc];

          buildPhase = ''
            runHook preBuild
            go build -buildmode=plugin -o ${pname}.so .
            runHook postBuild
          '';

          installPhase = ''
            runHook preInstall
            mkdir -p $out/lib
            cp ${pname}.so $out/lib/
            runHook postInstall
          '';

          # The unit tests live in internal/plugin and run in CI; running them
          # here would build the package twice for no extra signal.
          doCheck = false;

          meta = {
            description = "Bifrost gateway plugin";
            platforms = pkgs.lib.platforms.linux ++ pkgs.lib.platforms.darwin;
          };
        };

        devShells.default = pkgs.mkShell {
          packages = [
            pkgs.go_1_27
            pkgs.gcc # cgo, required by -buildmode=plugin
            pkgs.golangci-lint
            pkgs.gofumpt
            pkgs.govulncheck
            pkgs.gotestsum
            pkgs.lefthook
            pkgs.typos
            pkgs.act
            pkgs.actionlint
          ];
          # Never fetch a toolchain: the overlay above already pins the exact
          # compiler the host was built with, and silently downloading another
          # one is precisely how an unloadable .so gets produced.
          env.GOTOOLCHAIN = "local";
          shellHook = ''
            ${pkgs.lefthook}/bin/lefthook install >/dev/null 2>&1 || true
            echo "🔌 Bifrost plugin — Go $(go version 2>/dev/null || echo '(toolchain)')"
            command -v dg >/dev/null || echo "⚠️  dg not on PATH — decision-graph checks unavailable"
            echo ""
            echo "  Commands:"
            echo "    make plugin              build the .so"
            echo "    make verify              build + load it through ci/loader"
            echo "    go test -race ./...      run tests"
            echo "    golangci-lint run ./...  lint"
            echo "    gofumpt -l .             format check"
            echo "    govulncheck ./...        vuln scan"
            echo "    ./scripts/ci-local.sh    run CI jobs locally via act"
            echo ""
            echo "  The host must be a DYNAMIC build and pin the same core"
            echo "  version as go.mod — see README.md before deploying."
            echo ""
          '';
        };
      };
    };
}
