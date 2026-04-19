VERSION := $(shell cat VERSION | tr -d '[:space:]')
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/remaimber ./cmd/remaimber

install: build
	cp bin/remaimber ~/.local/bin/remaimber

test:
	go test ./... -v -count=1

clean:
	rm -rf bin/

.PHONY: build install test clean
