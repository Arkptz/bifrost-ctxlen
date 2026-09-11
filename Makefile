PLUGIN ?= bifrost-ctxlen
DIST   ?= dist

.PHONY: build plugin verify test lint fmt tidy vuln clean

build:
	go build ./...

# -buildmode=plugin compiles a DIRECTORY, so the target is `.` (the shim in
# main.go), and cgo is mandatory.
plugin:
	CGO_ENABLED=1 go build -buildmode=plugin -o $(DIST)/$(PLUGIN).so .

# The check that matters: build the .so and dlopen it. `go test` proves the
# hooks behave; only this proves the .so LOADS, which is where an ABI mismatch
# with the host's core version actually surfaces.
verify: plugin
	CGO_ENABLED=1 go build -o $(DIST)/loader ./ci/loader
	$(DIST)/loader $(DIST)/$(PLUGIN).so

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

fmt:
	gofumpt -l .

tidy:
	go mod tidy

vuln:
	govulncheck ./...

clean:
	rm -rf $(DIST)
