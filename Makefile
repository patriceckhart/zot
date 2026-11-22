# Local / untagged builds ship as 0.0.0. Release builds are driven by
# goreleaser which overrides VERSION from the git tag.
.DEFAULT_GOAL := build
VERSION ?= 0.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test lint fmt clean release run docs docs-local

docs:
	go run ./internal/cmd/fetchdocs

docs-local:
	go run ./internal/cmd/fetchdocs -allow-stale -timeout 15s

build: docs-local
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot ./cmd/zot

install: docs-local
	go install -ldflags "$(LDFLAGS)" ./cmd/zot

test:
	go test -race ./...

run: build
	./bin/zot

lint:
	go vet ./...
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt issues"; exit 1)

fmt:
	gofmt -w .

clean:
	rm -rf bin

release: docs
	@mkdir -p bin
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot-linux-amd64   ./cmd/zot
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot-linux-arm64   ./cmd/zot
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot-darwin-amd64  ./cmd/zot
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot-darwin-arm64  ./cmd/zot
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/zot-windows-amd64.exe ./cmd/zot
