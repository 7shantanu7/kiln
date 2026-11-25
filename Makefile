# kiln: build and test infrastructure for C++ monorepos (CMake + Ninja).
GO ?= go

.PHONY: build test lint bench

build:
	$(GO) build -o bin/ ./cmd/...

test:
	$(GO) test -race ./...

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	$(GO) vet ./...

# Full-size benchmark (500 targets, 4,250 translation units).
# Takes about an hour on 4 cores; results land in .bench-work/results.json.
bench: build
	bench/bench.sh .bench-work 20
