.PHONY: build dev test lint fmt clean

BIN ?= fedan

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) ./cmd/fedan

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
