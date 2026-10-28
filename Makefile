.PHONY: build dev test lint fmt clean

BIN ?= fedan
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/fedan

dev:
	FEDAN_DB_PATH=fedan-dev.db go run ./cmd/fedan

test:
	go test ./...

lint:
	gofmt -l cmd internal web | (! grep .)
	golangci-lint run ./...

fmt:
	gofmt -w .

clean:
	rm -f $(BIN)
