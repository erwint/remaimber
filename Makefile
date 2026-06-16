VERSION := $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/remaimber ./cmd/remaimber

install: build
	cp bin/remaimber ~/.local/bin/remaimber

test:
	go test ./... -v -count=1

clean:
	rm -rf bin/

.PHONY: build install test clean
