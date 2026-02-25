.PHONY: build run test vet clean

BINARY := relay
PKG := ./cmd/relay

build:
	go build -o $(BINARY) $(PKG)

run: build
	./$(BINARY) daemon

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

init: build
	./$(BINARY) init
