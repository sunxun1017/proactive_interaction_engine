# ADR 0006: Version Identity Calibration Artifacts

## Status

Accepted

## Context

The pinned CUDA biometric adapters emit geometry, embeddings, or unthresholded
scores. Model-card values, upstream examples, and model-only latency samples do
not establish a safe product operating point. Identity is used only to
personalize interaction; it is not authentication and cannot authorize access,
payments, device control, or private-memory disclosure by itself.

Calibration uses sensitive local datasets. Raw frames, audio, crops, PCM,
embeddings, templates, per-subject identifiers, sample identifiers, paths, and
per-trial scores must not enter Git, semantic audit, or aggregate calibration
outputs.

## Decision

- Define one strict JSON schema with integer `schema_version` equal to `1`.
  Reject duplicate, unknown, missing and null fields, string/number coercion,
  negative, floating-point or exponent numbers, trailing values, and unsupported
  versions. Schema v1 has no aliases, legacy reader, inferred values, or
  defaults.
- Bind exactly four ordered model roles to explicit model IDs and lowercase
  SHA-256 values: face detection, face embedding, face liveness, and speaker
  embedding. The model manifest remains the artifact allowlist; later
  production composition must compare these bindings with that manifest rather
  than treating the calibration artifact as a downloader or second allowlist.
- Restrict every artifact to `PERSONALIZATION_ONLY` and require
  `security_authentication_allowed` to be false.
- Bind an explicit `CUDA_ONLY` runtime, CUDA device index, Ubuntu amd64 NVIDIA
  target hardware profile, capture-device profiles, and exact software/runtime
  versions. `cuda_required` is true and `cpu_fallback_allowed` is false. A CPU
  deployment would require a new Provider, calibration artifact, and ADR; it is
  not a fallback path.
- Bind explicit visual and speaker preprocessing protocol IDs and every v1 rule
  value. Face similarity uses `AFFINE_COSINE_V1`, defined as `(cosine + 1) / 2`;
  this bounded score is not a probability. Detection score/NMS, face-liveness
  decision, speaker clip/VAD/filterbank, and enrollment rules remain explicit
  artifact values and have no defaults.
- Keep the three application thresholds distinct from Provider rules:
  face identification, speaker identification, and speaker verification. Store
  all scores and rates as integer parts per million and all latency as integer
  microseconds so canonicalization never depends on floating-point formatting.
  Production identity requires face liveness.
- Bind exactly five ordered logical Providers to aggregate end-to-end latency:
  face detection, face identification, face liveness, speaker identification,
  and speaker verification. Each record includes sample count, nearest-rank
  quantile rule, measurement boundary, p95, p99, observed maximum, and declared
  maximum. Values must satisfy
  `0 < p95 <= p99 <= observed maximum <= declared maximum`. Model inference
  timing alone is not Provider latency; the measurement boundary includes local
  capture or application work, the configured window, preprocessing, inference,
  RPC receipt, and cancellation terminal behavior.
- Bind the artifact to SHA-256 values for aggregate curve and aggregate latency
  reports. Compute the artifact hash over all validated semantic fields using
  fixed array order, lexicographically sorted JSON object keys, ASCII field
  values, integer numbers, and compact JSON encoding. The hash is returned by
  the loader and is not embedded recursively in the artifact.
- The offline calibration package is pure aggregation. FAR/FRR uses explicit
  genuine and impostor trial sets. Liveness uses APCER/BPCER rather than
  relabeling presentation attacks as identity errors. Rate conversion uses
  integer `ROUND_HALF_UP_V1`. Threshold grids are explicit and strictly
  increasing. Positive bounded latency samples use `NEAREST_RANK_V1` with
  `ceil(q * sample_count)` and produce only sample count, p95, p99, and observed
  maximum.
- Threshold selection is an operator decision. The tool verifies that all six
  supplied detection, NMS, liveness, face-identification,
  speaker-identification, and speaker-verification thresholds occur in their
  measured grids. It provides no EER, best-threshold, model-card, or implicit
  selection path.
- The aggregate package accepts anonymous in-memory integer scores only and
  emits thresholds, trial counts, error counts, and aggregate rates only. It
  has no model, media, filesystem, network, audit, subject, or template access.
  Synthetic test fixtures are contract evidence, not production thresholds.

## Consequences

- Production identity remains disabled until a separately reviewed artifact is
  generated from licensed, representative local data, the operator explicitly
  chooses all operating points and maximum latency values, and production
  composition verifies the artifact hash, model manifest, CUDA runtime, and
  hardware profile.
- This change provides only the schema loader, canonical hash, pure aggregate
  calculations, selection validation, and synthetic tests. It does not consume
  an artifact in `cmd/desktop`, execute models, access a real dataset, produce a
  production artifact, register a Provider, or claim Stage C3 completion.
- Dataset collection, consent, retention/deletion, representative trial design,
  confidence requirements, attack taxonomy, production clip rules, and latency
  budgets remain explicit review gates outside this slice.

## Rejected Alternatives

- Copying model-card or official-example thresholds: those values do not bind
  this deployment population, preprocessing, capture devices, or hardware.
- Automatically choosing EER or a numerically optimal threshold: it hides the
  product tradeoff between false acceptance and false rejection.
- Persisting per-trial scores or embeddings for later analysis: it expands the
  biometric-data surface and violates aggregate-only output.
- Floating-point JSON: cross-language formatting and rounding would weaken the
  reproducible hash.
- A permissive or version-guessing loader: it could silently change identity
  behavior or accept an artifact written for a different protocol.
- Silent CUDA-to-CPU fallback: it violates both the calibrated execution path
  and declared Provider latency.
