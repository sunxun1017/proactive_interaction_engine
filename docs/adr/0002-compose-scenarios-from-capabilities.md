# ADR 0002: Compose Scenarios from Typed Capabilities

## Status

Accepted

## Context

The product must support anonymous presence detection, hands-free VAD, face identification, speaker identification and verification, ASR, desktop avatar, TTS, and later robot adapters. Different scenarios need different subsets and deterministic fallbacks. Embedding model choices or device types in the core would break platform independence, while arbitrary runtime plugins or untyped metadata would weaken validation and privacy.

Biometric identity introduces sensitive templates, ambiguous candidates, multimodal conflicts, and per-subject memory isolation. It cannot be treated as a boolean device feature or as a direct model-to-policy path.

## Decision

Use a typed capability catalog and versioned scenario manifests.

- Providers register stable identity, protocol and implementation versions, typed capabilities, health, privacy class, latency and cancellation semantics.
- Scenarios declare required and optional capability types, one explicitly selected provider per capability, minimum identity assurance, and typed fallback behavior.
- Validate the complete selection before activation. Unknown, duplicate, incompatible, unhealthy, or unauthorized providers fail activation unless the scenario declares a safe fallback.
- Keep provider and scenario configuration in application/composition layers. Domain rules consume canonical observations and immutable capability-independent state.
- Keep face, voice, ASR, VAD, and hardware/model runtimes in isolated adapters or workers. Generated DTOs and model objects stay at the boundary.
- Route biometric evidence through an Identity Resolver before emitting a canonical subject identity observation. Ambiguity or conflicting modalities resolve to anonymous.
- Treat identification and verification as distinct capabilities. Do not use first-phase biometric results as security authentication.
- Default biometric capabilities to disabled. Require per-subject enrollment and consent; process locally, encrypt templates, discard raw enrollment media after extraction, and support deletion.
- Use explicit deployment-time provider selection first. Defer arbitrary code plugins and hot unload until a proven operational need exists.

## Consequences

- New capability providers can be added without changing proactive policy or behavior abstractions.
- Scenarios remain reproducible because provider selection and fallback are explicit and versioned.
- The platform needs contract conformance, health supervision, identity enrollment, resolver, and privacy UI boundaries.
- Multi-user identity will eventually add canonical identity domain language through a separately reviewed vertical slice; it does not allow platform DTOs, embeddings, or model types into the domain.
- Anonymous operation remains a complete supported path when biometric capabilities are disabled or degraded.

## Implementation Checkpoint

Stage C3 has reached a model-independent checkpoint, not stage completion.

Implemented:

- Strict scenario schema v2 validation, explicit Provider selection, and required Provider operational profiles. Scenario schema v2 does not mean Provider protocol or Protobuf package v2; cross-process contracts remain in `proactive.platform.v1`.
- Per-profile consent and enrollment metadata, encrypted catalog and template vault adapters, replacement, revocation, and retryable physical deletion.
- A deterministic Identity Resolver, bounded identification windows, application-owned speaker verification challenges, and synchronous desktop runtime and live-readiness seams.
- Five separate face detection, face identification, face liveness, speaker identification, and speaker verification evidence RPCs with lease, exact-Provider, permission, TTL, sequence, and deduplication gates.
- Fake and in-memory integration coverage across Registry, identity ingress, resolver/runtime, and encrypted deletion boundaries.

Not implemented:

- Real face, speaker, or liveness model workers, enrollment capture, template extraction, or enrollment-media disposal.
- A production master-key provider, desktop catalog/vault composition, or Camera/Microphone sharing between anonymous C2 and biometric workers.
- Enrollment and deletion UI, per-capability biometric runtime/enrollment status, or production desktop identity activation. `cmd/desktop.Build` still runs with identity disabled.
- Canonical subject identity observations, private-memory gates, personalized welcome, or shared-household composition; those belong to the separately reviewed C4 slice.

## Rejected Alternatives

- A universal `map<string, value>` capability registry: weak schemas and ambiguous compatibility.
- Direct model access to Engine state, policy, memory, or ActionDriver: violates dependency and safety boundaries.
- Workers emitting final `UserReply` or trusted subject IDs without application gates: bypasses response-window and identity policy.
- Arbitrary dynamic plugins in the first implementation: unnecessary lifecycle, trust, and compatibility complexity.
- Storing raw media or embeddings in the semantic audit: violates data minimization and replay boundaries.
