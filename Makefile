LDFLAGS := -s -w
BUILD   := CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)"

.PHONY: build size test test-matrix bench agent-test clean

build:
	$(BUILD) -o bin/pggo ./cmd/pggo

# Reproducible size report for the release targets.
size:
	@for t in linux/amd64 linux/arm64 darwin/arm64; do \
	  GOOS=$${t%/*} GOARCH=$${t#*/} $(BUILD) -o bin/pggo-$${t%/*}-$${t#*/} ./cmd/pggo; \
	  printf '%-14s %9d bytes  %9d gzip -9\n' $$t $$(wc -c < bin/pggo-$${t%/*}-$${t#*/}) $$(gzip -9 -c bin/pggo-$${t%/*}-$${t#*/} | wc -c); \
	done

test:
	go vet ./...
	go test ./internal/... ./cmd/...

# Integration tests against PostgreSQL 16, 17, 18 and 19beta1 in Docker.
test-matrix:
	scripts/test-matrix.sh

bench: build
	scripts/bench.sh

agent-test: build
	agent_tests/run.sh

clean:
	rm -rf bin
	scripts/pg-down.sh
