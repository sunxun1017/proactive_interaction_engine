GO ?= go
BUF ?= buf

.PHONY: fmt fmt-check vet test race architecture proto proto-generate proto-check web-build web-check staticcheck check simulator conformance docker-check

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

proto-generate:
	$(BUF) generate

proto-check: proto proto-generate
	git diff --exit-code -- gen

web-build:
	mkdir -p dist/web
	GOOS=js GOARCH=wasm $(GO) build -trimpath -ldflags='-s -w' -o dist/web/app.wasm ./web/desktop/cmd/panel
	go_root="$$( $(GO) env GOROOT )"; cp "$$go_root/lib/wasm/wasm_exec.js" dist/web/wasm_exec.js

web-check:
	GOOS=js GOARCH=wasm $(GO) build -trimpath -o /tmp/proactive-panel.wasm ./web/desktop/cmd/panel
	GOOS=js GOARCH=wasm $(GO) vet ./web/desktop/cmd/panel

staticcheck:
	staticcheck ./...

check: fmt-check vet test race architecture

simulator:
	$(GO) run ./cmd/simulator

conformance:
	$(GO) run ./cmd/conformance

docker-check:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace golang:1.23-alpine sh -c 'gofmt -w . && go vet ./... && go test ./... && go test -race ./...'
