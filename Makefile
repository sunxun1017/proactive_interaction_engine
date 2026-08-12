GO ?= go
BUF ?= buf

.PHONY: fmt fmt-check vet test race architecture proto staticcheck check simulator conformance docker-check

fmt:
	gofmt -w $$(find . -type f -name '*.go' -not -path './gen/*')

fmt-check:
	@test -z "$$(gofmt -l $$(find . -type f -name '*.go' -not -path './gen/*') | tee /dev/stderr)"

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

architecture:
	$(GO) test ./tests/architecture

proto:
	$(BUF) lint

staticcheck:
	staticcheck ./...

check: fmt-check vet test race architecture

simulator:
	$(GO) run ./cmd/simulator

conformance:
	$(GO) run ./cmd/conformance

docker-check:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace golang:1.23-alpine sh -c 'gofmt -w . && go vet ./... && go test ./... && go test -race ./...'
