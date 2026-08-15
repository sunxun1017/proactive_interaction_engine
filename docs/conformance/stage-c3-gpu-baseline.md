# Stage C3 GPU Baseline

This record captures the first reproducible CUDA conformance run for the
checked-in model pins. It is engineering evidence, not a production threshold
or a claim that Stage C3 enrollment and Provider composition are complete.

## Environment

- Date: 2026-08-15
- GPU: NVIDIA GeForce RTX 4060 Laptop GPU, compute capability 8.9
- Driver: 580.173.02
- Python: 3.10.20
- PyTorch: 2.11.0+cu128; CUDA 12.8; cuDNN 9.19
- ONNX Runtime GPU: 1.23.2
- OpenCV: 4.10.0

The environment was created from `environment.gpu.yml`. A CUDA tensor
multiplication completed on `cuda:0`.

## Visual artifacts

The manifest-pinned YuNet, SFace and anti-spoof ONNX files matched exact size
and SHA-256. Each model created a CUDA-only ONNX Runtime session with CPU EP
fallback disabled. Profiling showed every executed model node assigned to
`CUDAExecutionProvider`.

The conformance cases proved the pinned YuNet 640x640 input and blank-frame
decode, one finite L2-normalized 128-dimensional SFace embedding, and two
finite anti-spoof probabilities summing to one. A licensed same-person versus
different-person face fixture is still required before setting or validating
recognition thresholds.

## Speaker artifact

The ModelScope ERes2Net checkpoint matched 221,210,095 bytes and SHA-256
`ad78a02cab9dc23385c3fe235d1c3a370c0bd89ef192d0250546b370b0b8f227`.
Restricted `weights_only=True` loading produced 809 state entries; strict
loading matched the vendored 55,165,024-parameter architecture.

The official example inputs were also pinned and verified:

- `speaker1_a_cn_16k.wav`: 118,932 bytes, SHA-256 `5f20ce0ddc378ca3239d3ce864b1142726a46a1221ae553912e4e142045df58b`
- `speaker1_b_cn_16k.wav`: 157,058 bytes, SHA-256 `20745dc08a4281894d146140b99b9ef7417ac681119b7f7202f553cdf1a85f65`
- `speaker2_a_cn_16k.wav`: 170,028 bytes, SHA-256 `8a6cffa452df32ef10503f7992f22ffcdd7f16c4e0273d13311bc5cdcb13abf4`

Each file is available under the pinned ModelScope path
`https://www.modelscope.cn/models/iic/speech_eres2net_sv_zh-cn_16k-common/resolve/v1.0.5/examples/`.

Using the three official 16 kHz examples and five repeated `cuda:0` inference
runs:

- same-speaker normalized score: 0.8563388894
- different-speaker normalized score: 0.5143195797
- deterministic maximum absolute embedding delta: 0.0
- model load: 1669.83 ms
- steady p50 inference: 30.66 ms
- five-sample p95/p99 including first-run warm-up: 276.16 ms
- peak CUDA allocation: 455,167,488 bytes
- process peak RSS: 1,104,792 KiB

The score is the deterministic affine mapping `(cosine + 1) / 2`; it is not a
probability. These three examples cannot define identification or verification
thresholds. Provider `maximum_latency` must also use a later end-to-end capture,
window, RPC and cancellation measurement rather than this model-only sample.
