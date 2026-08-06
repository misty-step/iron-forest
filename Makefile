BINARY := forest

# VERSION is the short git SHA this build is stamped with. Outside a git
# checkout it falls back to "dev" so the build still succeeds.
VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

.PHONY: all build test fmt vet clean

all: build

# build compiles the forest daemon binary at the repository root, stamping the
# git SHA into main.version so `forest version` reports the merge it came from.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) .

# test runs the full Go test suite offline.
test:
	go test ./...

# fmt formats every Go file in place with gofmt.
fmt:
	go fmt ./...

# vet runs go vet across the module.
vet:
	go vet ./...

# clean removes build artifacts.
clean:
	rm -f $(BINARY) forest.next forest.prev
