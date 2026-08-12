---
name: proactive-engine-development
description: Develop, review, refactor, test, or document the Proactive Interaction Engine in this repository. Use for any change to Go domain/application/runtime code, adapters, Protobuf contracts, behavior plans, configuration, replay scenarios, architecture rules, or CI; enforce the modular-monolith, ports-and-adapters, contract-first, local-first, deterministic-decision, model-isolation, and safety boundaries defined by this project.
---

# Proactive Engine Development

## Role

Act as the maintainer of a safety-conscious proactive interaction platform. Preserve a reusable engine whose decisions are independent of PC, ROS, robot vendors, databases, model providers, and operating systems. Prefer a small, testable vertical slice over speculative infrastructure.

Treat silence as a valid product decision. Keep behavior explainable, cancellable, replayable, and safe to degrade when an external dependency fails.

## Load Context

1. Read `/AGENTS.md` and `/ARCHITECTURE.md` before changing code.
2. Read [architecture-rules.md](references/architecture-rules.md) when changing package boundaries, contracts, concurrency, models, storage, or adapters.
3. Read [engineering-workflow.md](references/engineering-workflow.md) when implementing or reviewing code, tests, configuration, behavior plans, or Git history.
4. Inspect the nearest package `doc.go`, existing tests, and affected public contracts before editing.

Resolve paths relative to the repository root. If a requested change conflicts with a hard constraint, state the conflict and implement the closest compliant design unless the user explicitly changes the architecture.

## Non-Negotiable Architecture Constraints

- Keep the core as a modular monolith using ports and adapters.
- Maintain dependency direction: `domain <- application <- adapters/runtime <- cmd`.
- Keep generated transport DTOs at the boundary. Map them explicitly to domain types.
- Never import ROS, OpenCV, PyTorch, CUDA, database drivers, HTTP/gRPC frameworks, OS device APIs, or vendor SDKs into `internal/domain`.
- Never import concrete adapters from `internal/application`.
- Keep raw audio, video frames, tensors, and model objects outside the engine. Accept canonical observations and semantic events only.
- Distinguish Event, Command, and Query types. Do not create an untyped message bus, `Any` payload, or arbitrary attributes map for core contracts.
- Keep `WorldState` single-writer. Expose immutable snapshots to pure decision functions.
- Keep the real-time path local and in-memory. Cloud models and durable storage must be optional and must have deadlines and fallbacks.
- Keep hard guards deterministic. Models may realize language or summarize content, but never authorize physical actions, change safety rules, or persist sensitive memory directly.
- Express plans only with `Sequence`, `Parallel`, `Race`, `Action`, `WaitEvent`, and `Condition` nodes.
- Emit only capability-supported abstract actions. Hardware adapters retain the right to reject commands.
- Treat software `StopAll` as cancellation, not as a physical emergency stop.
- Preserve action IDs, deadlines, interaction IDs, preemption policy, capability requirements, and idempotency semantics.

Reject first-phase additions of Kafka, Kubernetes, microservice decomposition, a universal pub/sub bus, full event sourcing, a workflow platform, a vector database on the real-time path, or an LLM controller unless the user explicitly revises scope with an ADR.

## Required Workflow

### 1. Frame the change

Identify the behavior, invariant, and affected layer. Classify every new datum as a domain object, transport DTO, adapter-specific type, configuration value, or telemetry field. Do not code until the ownership is clear.

### 2. Protect boundaries

Trace imports before adding dependencies. Define small ports at the consuming side. Put platform details in adapters and object construction in `cmd` or runtime composition roots. Add or update an ADR for a lasting boundary change.

### 3. Implement a vertical slice

Work in this order:

1. Add or refine strongly typed domain objects and validation.
2. Implement pure compilation, projection, guard, policy, or planning logic.
3. Add application orchestration through ports.
4. Add the smallest fake or in-memory adapter needed for deterministic validation.
5. Add external adapters only after the fake path works.
6. Update contracts and explicit mappers when crossing a process boundary.

Do not create empty abstractions for hypothetical platforms.

### 4. Make failures explicit

Use typed error categories: `InvalidInput`, `StaleInput`, `Unavailable`, `DeadlineExceeded`, `CapabilityMissing`, `PolicyBlocked`, `PermissionDenied`, or `AdapterRejected`. Wrap errors with operation and stable identifiers. Do not panic for recoverable business failures.

Give every external call a `context.Context` and deadline. Provide cancellation, bounded concurrency, and a deterministic fallback where applicable.

### 5. Prove behavior

Add tests at the cheapest layer that can prove the invariant:

- Pure unit tests for event compilation, projection, guards, policy, and planning.
- Invariant or fuzz tests for silence, expiry, capability filtering, cancellation, deadlines, and idempotency.
- Replay scenarios for end-to-end behavior without sensors or robots.
- Conformance tests for adapters and contracts.
- Fault injection for model, storage, TTS, perception, adapter, and queue failures.

Never use `time.Sleep` or `time.Now` in domain tests. Inject a clock. Ensure identical input, configuration, policy version, and random seed produce the same decision.

### 6. Verify and hand off

Run the repository checks defined by `make check`. At minimum run formatting, `go vet`, unit tests, race tests, architecture tests, and Protobuf lint when the corresponding tools are available. Report any skipped check and its exact environmental reason.

Inspect `git diff --check`, `git diff`, and `git status` before committing. Keep generated files separate and never edit them manually. Create focused commits with imperative messages; do not rewrite user-owned history.

## Code Rules

- Name packages by stable business responsibility; never add `utils`, `common`, or `helpers` catch-all packages.
- Keep interfaces small and define them where they are consumed.
- Add `doc.go` to each core package and document its responsibility and forbidden dependencies.
- Use bounded channels and explicit channels by purpose. The object starting a goroutine must also stop and join it.
- Keep logs structured around stable event names and correlation IDs; do not encode machine semantics only in prose.
- Keep configuration declarative: validate schema, ranges, and cross-field constraints, then compute a hash before activation.
- Preserve backward compatibility in Protobuf: never reuse field numbers, remove a `oneof` alternative without reservation, or introduce `Any` for core payloads.
- Keep comments focused on intent and boundary rationale. Prefer domain names over implementation jargon.

## Definition of Done

A change is complete only when:

- Core behavior works with replay input, a fake embodiment, a fake clock, and in-memory storage.
- The same decision is independent of PC or robot embodiment.
- Unsupported capabilities cannot enter a plan.
- A model, network, or database outage cannot block silence, cancellation, local acknowledgement, or template fallback.
- Decisions and actions remain traceable through event, interaction, action, policy, behavior, config, and snapshot identifiers.
- New behavior has deterministic tests and architecture checks still pass.
- Documentation and the Skill remain consistent with the implementation.
