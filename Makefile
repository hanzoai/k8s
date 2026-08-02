# One entry point. `make test` runs every gate HIP-0106 §8 requires, and hanzo.yml's
# test: block invokes exactly this target — a gate CI does not call is a comment.
GO ?= go
export GOWORK = off
export GOPRIVATE = github.com/hanzoai/*

.PHONY: all build generate test vet clean

all: build

build:
	$(GO) build -trimpath -o k8s .

# The projections. Both are FILES written by the binary from its own live router, and
# both are committed — the suite regenerates and fails on any diff, so a committed
# artifact is never a golden nothing forces back to source.
generate: build
	$(GO) generate ./...
	./k8s declare k8s.plugin.json
	./k8s openapi openapi.json

vet:
	$(GO) vet ./...

test: vet
	$(GO) test ./... -count=1

clean:
	rm -f k8s
