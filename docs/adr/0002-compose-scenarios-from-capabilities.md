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
- Use one supervised camera owner and one supervised microphone owner, while registering the seven vision/audio capabilities as independent single-capability logical Providers and leases. A shared process is not permission or health equivalence.
- Deliver application-issued biometric windows and challenges through fixed capacity-one latest-state streams on the existing private UDS. Do not replay work after reconnect or turn this boundary into a generic event bus. Verification work carries bounded encoded material without exposing its expected profile or template reference.
- Use explicit deployment-time provider selection first. Defer arbitrary code plugins and hot unload until a proven operational need exists.

## Consequences

- New capability providers can be added without changing proactive policy or behavior abstractions.
- Scenarios remain reproducible because provider selection and fallback are explicit and versioned.
- The platform needs contract conformance, health supervision, identity enrollment, resolver, and privacy UI boundaries.
- Multi-user identity will eventually add canonical identity domain language through a separately reviewed vertical slice; it does not allow platform DTOs, embeddings, or model types into the domain.
- Anonymous operation remains a complete supported path when biometric capabilities are disabled or degraded.

## Implementation Checkpoint

Stage C3 has reached a model-enabled foundation checkpoint, not stage completion.

Implemented:

- Strict scenario schema v2 validation, explicit Provider selection, and required Provider operational profiles. Scenario schema v2 does not mean Provider protocol or Protobuf package v2; cross-process contracts remain in `proactive.platform.v1`.
- Per-profile consent and enrollment metadata, encrypted catalog and template vault adapters, replacement, revocation, and retryable physical deletion.
- A deterministic Identity Resolver, bounded identification windows, application-owned speaker verification challenges, and synchronous desktop runtime and live-readiness seams.
- Five separate face detection, face identification, face liveness, speaker identification, and speaker verification evidence RPCs with lease, exact-Provider, permission, TTL, sequence, and deduplication gates.
- Two fixed private identity worker-control streams on the existing UDS. They retain only capacity-one latest desired state, do not replay after reconnect, revalidate only task-relevant logical Providers, and structurally omit profile/template references from verification work.
- A two-process Supervisor foundation that derives the exact authorized logical-Provider subset, restarts and revokes the complete device group on effective permission changes, gives the new process a fresh instance ID, and passes only the validated subset to the child.
- Fake and in-memory integration coverage across Registry, identity ingress, resolver/runtime, and encrypted deletion boundaries.
- A production Ubuntu Secret Service master-key adapter, one pinned CUDA Conda environment, a strict offline model artifact manifest, CUDA-only YuNet/SFace/anti-spoof/ERes2Net model adapters, and fixed face/speaker template codecs. Real target-GPU conformance verifies artifact integrity and execution placement but does not define product thresholds.
- Strict opt-in desktop composition for Secret Service, crash-safe catalog/vault recovery, identity runtime, evidence ingress, and live readiness. The checked-in production configuration remains identity-disabled.

Not implemented:

- Supervised face, speaker, or liveness Provider execution loops, enrollment capture orchestration, or enrollment-media disposal.
- Deployment calibration artifacts and end-to-end Provider latency declarations bound to the selected model and preprocessing versions.
- Camera/Microphone device-owner stacks that share bounded media internally while preserving seven independent logical Provider leases.
- Enrollment and deletion UI, per-capability biometric runtime/enrollment status, or a checked-in production identity activation configuration.
- Canonical subject identity observations, private-memory gates, personalized welcome, or shared-household composition; those belong to the separately reviewed C4 slice.

## Rejected Alternatives

- A universal `map<string, value>` capability registry: weak schemas and ambiguous compatibility.
- Direct model access to Engine state, policy, memory, or ActionDriver: violates dependency and safety boundaries.
- Workers emitting final `UserReply` or trusted subject IDs without application gates: bypasses response-window and identity policy.
- Arbitrary dynamic plugins in the first implementation: unnecessary lifecycle, trust, and compatibility complexity.
- Storing raw media or embeddings in the semantic audit: violates data minimization and replay boundaries.
