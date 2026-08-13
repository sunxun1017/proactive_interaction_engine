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
- Route biometric candidates through an Identity Resolver before emitting a canonical subject identity observation. Ambiguity or conflicting modalities resolve to anonymous.
- Treat identification and verification as distinct capabilities. Do not use first-phase biometric results as security authentication.
- Default biometric capabilities to disabled. Require per-subject enrollment and consent; process locally, encrypt templates, discard raw enrollment media after extraction, and support deletion.
- Use explicit deployment-time provider selection first. Defer arbitrary code plugins and hot unload until a proven operational need exists.

## Consequences

- New capability providers can be added without changing proactive policy or behavior abstractions.
- Scenarios remain reproducible because provider selection and fallback are explicit and versioned.
- The platform needs contract conformance, health supervision, identity enrollment, resolver, and privacy UI boundaries.
- Multi-user identity will eventually add canonical identity domain language through a separately reviewed vertical slice; it does not allow platform DTOs, embeddings, or model types into the domain.
- Anonymous operation remains a complete supported path when biometric capabilities are disabled or degraded.

## Rejected Alternatives

- A universal `map<string, value>` capability registry: weak schemas and ambiguous compatibility.
- Direct model access to Engine state, policy, memory, or ActionDriver: violates dependency and safety boundaries.
- Workers emitting final `UserReply` or trusted subject IDs without application gates: bypasses response-window and identity policy.
- Arbitrary dynamic plugins in the first implementation: unnecessary lifecycle, trust, and compatibility complexity.
- Storing raw media or embeddings in the semantic audit: violates data minimization and replay boundaries.
