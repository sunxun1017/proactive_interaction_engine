# Engineering Workflow

## Change Checklist

1. Restate the user-visible outcome and safety invariant.
2. Identify unresolved choices that could materially change the result and ask the user one concise question at a time.
3. Locate the current owner and nearest tests.
4. Check import direction and contract compatibility.
5. Write a failing deterministic test or replay fixture.
6. Implement the smallest domain change.
7. Connect it through an application port and fake adapter.
8. Add platform or process-boundary code last.
9. Capture reusable user answers in the Skill, relevant reference, architecture document, ADR, contract, configuration, or test.
10. Run `make check` and inspect Git changes.

## Decision Capture

Persist a user answer only when it will guide future work. Put short universal rules in `SKILL.md`; put detailed implementation guidance in this reference; put system boundaries in `ARCHITECTURE.md` or an ADR; encode observable behavior in tests and configuration. Update all affected sources of truth together instead of copying the same prose into multiple files.

When a new answer supersedes an old rule, replace the old rule and update its tests. Do not append contradictory history to the Skill. Record architectural history in an ADR when the reasoning remains important.

## Go Conventions

- Return zero-value-friendly immutable value objects where practical.
- Validate at ingress and again at trust boundaries; keep pure policy code free of I/O, clocks, randomness, and environment access.
- Wrap errors as `operation + stable ID + cause` and preserve typed causes with `%w`.
- Accept `context.Context` as the first parameter of external calls; never store it in a struct.
- The creator of a channel owns closing it. Do not close receive-only channels from consumers.
- Avoid goroutines in domain packages. Runtime owners must expose shutdown and wait semantics.
- Use table-driven tests and `t.Parallel()` only when the subject has no shared mutable state.
- Use fake clocks and deterministic IDs in tests; never wait for wall time.

## Contract Changes

Before editing `.proto` files, classify the change as additive, compatible migration, or breaking. Prefer additive optional fields and new `oneof` alternatives. Reserve deleted numbers and names. Update adapter conformance fixtures and mappers together.

Run:

```bash
buf lint
buf breaking --against <baseline>
```

Do not manually edit files below `gen/`.

## Configuration Changes

Add parameters only for tuning, not branching business programs. Define defaults, units, range constraints, cross-field constraints, and fallback behavior. Ensure the validated configuration has a stable hash recorded in each episode.

## Test Selection

| Change | Minimum proof |
| --- | --- |
| Domain rule | Unit test plus affected invariant |
| Time threshold | Fake-clock boundary tests |
| Behavior action | Capability-filter test and replay test |
| Adapter | Conformance and failure test |
| Cancellation | Race test and terminal-status assertion |
| Protobuf | Lint, compatibility check, mapper test |
| Concurrency | Race test, overload test, shutdown test |
| Architecture | Import-boundary test |

## Git Protocol

- Inspect status before editing and preserve unrelated user changes.
- Stage only files belonging to the requested change.
- Run `git diff --check` before committing.
- Use an imperative, scoped commit message such as `feat: add return greeting vertical slice`.
- Never force-push, reset hard, or rewrite existing user commits without explicit approval.
