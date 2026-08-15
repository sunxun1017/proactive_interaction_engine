# ADR 0004: Run Biometric Inference on Explicit CUDA

## Status

Accepted

## Context

The first Ubuntu target has an NVIDIA GPU, and the selected face, liveness and
speaker models are too expensive to treat CPU execution as an equivalent
production path. A silent execution-provider fallback would make latency,
thermal behavior and Provider declarations depend on accidental host state.
Downloading mutable model names at worker startup would also make results and
licenses impossible to reproduce.

GPU loss must not become a platform-wide outage. Anonymous presence, VAD,
templates, silence and P0 cancellation are valid without biometric identity.

## Decision

- Keep one Git-managed Conda definition, `environment.gpu.yml`, for all Python
  media and biometric workers. It pins Python 3.10, CUDA-enabled PyTorch,
  torchaudio, ONNX Runtime GPU, OpenCV preprocessing, gRPC, Protobuf, NumPy and
  WebRTC VAD. Do not maintain a second requirements file with overlapping
  versions.
- Require an explicit CUDA device index at every biometric model boundary.
  ONNX Runtime sessions enable only `CUDAExecutionProvider`, disable API and
  session CPU fallback, verify the activated device, and support a profiling
  audit that rejects any executed model node assigned outside CUDA. PyTorch
  speaker inference requires CUDA tensors on the requested device. There is no
  production CPU inference fallback.
- Treat missing CUDA, a wrong device, runtime load failure, digest mismatch or
  execution-provider fallback as model unavailability. The owning biometric
  Provider becomes unavailable/unhealthy and the scenario follows its declared
  anonymous fallback. The failure must not stop Camera presence, VAD, reviewed
  templates, silence, or P0 cancellation.
- Keep model binaries out of Git. `workers/models/manifest.v1.json` is the sole
  runtime artifact allowlist and pins an immutable URL, exact byte size,
  SHA-256 and license evidence. The downloader accepts only approved HTTPS
  hosts and redirects, streams into a same-directory temporary file, verifies
  all bytes, and atomically installs the artifact. Workers run offline and
  never fetch or execute remote code.
- Model adapters emit geometry, embeddings or unthresholded scores only. Face,
  liveness, speaker-identification and speaker-verification thresholds belong
  to a separately reviewed, versioned calibration artifact bound to the model
  SHA, preprocessing protocol and deployment hardware. Official examples and
  benchmark metrics are conformance evidence, not product thresholds.
- Fixed-width face and speaker template codecs include the exact model digest,
  reject malformed/non-finite/non-normalized values and never use Pickle. The
  encrypted biometric vault remains the only durable owner of encoded
  templates.

## Consequences

- `make media-env` requires Conda, a compatible NVIDIA driver and a visible
  CUDA device. It performs a real CUDA tensor operation rather than checking
  package imports alone.
- Visual inference uses ONNX Runtime GPU while speaker inference uses PyTorch
  CUDA; these remain worker implementation details and never enter Go domain or
  application packages.
- The current slice proves pinned model loading and CUDA execution but does not
  yet provide capture sharing, enrollment orchestration, production thresholds
  or supervised Provider processes. Biometric activation therefore remains
  disabled in the production desktop composition.
- Supporting a CPU-only deployment would require a new explicit Provider,
  operational profile, calibration artifact and ADR. It is not a compatibility
  fallback of the CUDA Provider.

## Rejected Alternatives

- Silent CUDA-to-CPU fallback: violates declared latency and makes degraded
  behavior host-dependent.
- A model downloader inside each worker: duplicates trust policy and permits
  runtime network access.
- Floating model revisions or framework hubs with remote code: break replay,
  review and supply-chain verification.
- Thresholds copied from model cards or three example files: do not represent
  the target camera, microphone, household or chosen false-accept tradeoff.
