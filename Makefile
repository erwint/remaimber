VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/remaimber ./cmd/remaimber

install: build
	@mkdir -p ~/.local/bin
	@# Replace atomically. Writing over the binary in place corrupts the mapping
	@# of any copy already running — and the hooks run remaimber in the
	@# background constantly — after which macOS SIGKILLs every new invocation.
	@cp bin/remaimber ~/.local/bin/.remaimber.new
	@mv -f ~/.local/bin/.remaimber.new ~/.local/bin/remaimber
	@echo "installed $$(~/.local/bin/remaimber --version)"

test:
	go test ./... -v -count=1

clean:
	rm -rf bin/

.PHONY: build install test clean
