.PHONY: test build clean

test:
	go test ./...

build:
	go build ./cmd/server ./cmd/bridge

clean:
	go clean
