BIN     := agentmutex
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build install test lint clean

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) .

install:
	go install -ldflags '$(LDFLAGS)' .

test:
	go test ./... -timeout 5m

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo 'gofmt: files need formatting' && exit 1)

clean:
	rm -f $(BIN)
