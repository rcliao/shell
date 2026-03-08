.PHONY: build run test vet clean watch

BINARY := shell
PKG := ./cmd/shell

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

watch: build
	./$(BINARY) daemon --watch

init: build
	./$(BINARY) init
