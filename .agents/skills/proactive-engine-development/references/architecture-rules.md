# Architecture Rules

## Dependency Map

```text
internal/domain
      ^
internal/application
      ^
adapters and internal/runtime
      ^
cmd composition roots
```

Allowed domain flow:

```text
Observation -> SemanticEvent -> WorldSnapshot
            -> Opportunity -> Decision -> BehaviorPlan
            -> ActionCommand -> ActionStatus -> Outcome -> MemoryCandidate
```

External boundaries use Protobuf DTOs and explicit mappers. Internal calls use Go types and interfaces.

## Ownership

| Concern | Owner | Must not own |
| --- | --- | --- |
| Sensor/vendor data conversion | Input adapter | Policy or state mutation |
| Temporal fact compilation | Event compiler | Device APIs |
| Current world state | Single engine loop/projector | External I/O |
| Opportunity, guards, policy | Decision domain | Language realization or hardware mapping |
| Abstract behavior | Behavior planner | Joint, topic, animation, or SDK details |
| Scheduling and cancellation | Executor | Physical emergency-stop guarantees |
| Hardware mapping | Embodiment adapter | Product policy |
| Model inference | Isolated worker/adapter | Direct behavior execution |
| Persistence | Repository adapter | Real-time availability requirement |

## Event, Command, Query

- Event: immutable fact that already happened, such as `PersonReturned`.
- Command: requested effect that may be rejected, cancelled, or fail, such as `AttendUser`.
- Query: read-only request, such as `GetCurrentWorldState`.

Use separate types and APIs. Do not route all three through one JSON envelope or global bus.

## Runtime Invariants

- Process control/cancellation at P0, action status at P1, semantic work at P2, and telemetry/memory at P3.
- Keep queues bounded. Never drop control commands or action terminal states.
- Coalesce continuous perception to latest state; compile semantic transitions after temporal thresholds.
- Only one goroutine mutates `WorldState`; other code reads snapshots.
- Keep the event-to-first-action path independent of cloud and storage.
- Use the same `trace_id`, `interaction_id`, `event_id`, and `action_id` across telemetry.

Initial target capacities: control 32, semantic event 256, action status 256, model request 16, telemetry 1024. Continuous state has no queue and retains only the latest value.

## Determinism and Replay

Record `policy_version`, `behavior_version`, `config_hash`, `snapshot_hash`, `model_version`, `random_seed`, and `trace_id` with every decision. Given the same semantic event log, configuration, versions, and seed, reproduce the same decision.

Persist semantic audit records and periodic snapshots; do not require full event sourcing. Do not record raw audio or video by default.

## Models and Memory

Model calls require a deadline, cancellation, maximum concurrency, request budget, fallback, and circuit breaker. A model can realize text, interpret dialogue, summarize facts, or propose memory candidates. Deterministic policy and memory gates validate every model result.

Never allow a model to move a robot, enable sensors, contact a person, change a safety rule, or store sensitive memory directly.

## Adapter Contract

Input adapters prove registration, heartbeat, monotonic sequence, deduplication, TTL, reconnect, confidence validation, and protocol version handling.

Output adapters prove capability declaration, unsupported-action rejection, action-ID idempotency, cancellation, deadline enforcement, ordered status transitions, safe disconnect, and lease expiry.

Explicit `USER_REJECTED` control follows two phases. The concurrent P0 phase cancels the active processing context and calls `ActionDriver.StopAll`; it must not mutate `WorldState` or Episode state. After the active processing goroutine is joined, the Runner serially commits the rejection semantic event, ends the matching Episode with a `REJECTED` Outcome, and projects the subject-local cooldown. The default explicit-rejection cooldown is 30 minutes and remains configuration-driven. Commit the user fact even when `StopAll` fails. Shutdown cancellation never creates a rejection.

Do not claim `NO_RESPONSE` support until `WaitEvent` execution, its deterministic timeout, Episode completion, Outcome audit, and replay proof are implemented together.

ROS messages, vendor types, model tensors, database rows, and generated Protobuf messages never cross into the domain layer.
