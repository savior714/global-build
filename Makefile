# Makefile for global-build

GO ?= go
BIN := global-build
CMD := ./cmd/global-build

.PHONY: build install test test-race vet tidy fmt clean

build:
	$(GO) build -o $(BIN) $(CMD)

install:
	$(GO) install $(CMD)

# Tests run with -race: the runner/ownership/opencode packages have real
# concurrency (worktree locks, tracker, lease contention).
test: test-race

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

clean:
	rm -f $(BIN)
