BINARY     = mantis
BUILD_DIR  = ./bin
MODULE     = github.com/mantis-dev/mantis
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS    = -ldflags "-X main.version=$(VERSION)"

.PHONY: build install clean test lint run

## build: compile the mantis binary to ./bin/mantis
build:
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/mantis

## install: install mantis to $GOPATH/bin
install:
	go install $(LDFLAGS) ./cmd/mantis

## clean: remove build artifacts
clean:
	rm -rf $(BUILD_DIR)

## test: run all tests
test:
	go test ./...

## lint: run go vet
lint:
	go vet ./...

## run: build and open the AI assistant
run: build
	$(BUILD_DIR)/$(BINARY)
