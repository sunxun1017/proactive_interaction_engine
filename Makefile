GO ?= go
BUF ?= buf
CONDA ?= conda

MEDIA_ENV := $(CURDIR)/.conda-gpu
MEDIA_PYTHON := $(MEDIA_ENV)/bin/python
MEDIA_CUDA_DEVICE ?= 0

.PHONY: fmt fmt-check vet test race architecture proto proto-generate proto-check web-build web-check media-env media-test staticcheck check simulator conformance docker-check

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

media-env:
	$(CONDA) env update --prefix "$(MEDIA_ENV)" --file environment.gpu.yml --prune
	$(MEDIA_PYTHON) -c 'import cv2, grpc, numpy, onnxruntime, torch, torchaudio, webrtcvad; device = torch.device("cuda:$(MEDIA_CUDA_DEVICE)"); assert torch.cuda.is_available() and device.index < torch.cuda.device_count(); assert (torch.tensor([3.0], device=device) * 4).item() == 12.0; assert "CUDAExecutionProvider" in onnxruntime.get_available_providers()'

media-test:
	PYTHONPATH="$(CURDIR)/gen/python:$(CURDIR)" $(MEDIA_PYTHON) -m unittest discover -s workers -p 'test_*.py'

staticcheck:
	staticcheck ./...

check: fmt-check vet test race architecture

simulator:
	$(GO) run ./cmd/simulator

conformance:
	$(GO) run ./cmd/conformance

docker-check:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace golang:1.23-alpine sh -c 'gofmt -w . && go vet ./... && go test ./... && go test -race ./...'
