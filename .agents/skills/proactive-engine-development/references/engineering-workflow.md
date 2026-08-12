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

## Simplicity and Compatibility

- Start with the direct implementation. Extract an interface, generic helper, option type, or framework only when a real boundary or repeated use demonstrates the need.
- Optimize for one obvious path through the code. Split complex conditions by domain meaning and return early when it improves readability.
- Remove dead code, commented-out code, obsolete feature flags, unused configuration, and duplicate implementations in the same change that makes them obsolete.
- Do not add compatibility aliases, dual reads/writes, schema guessing, permissive decoding, or silent type coercion for hypothetical users.
- Treat unexported and unreleased internal APIs as changeable. Update their callers and tests together instead of carrying a shim.
- Preserve compatibility only for documented external versions or an explicit migration requirement. Isolate it at the boundary, make version selection explicit, add contract tests and telemetry, and document its owner and removal condition.
- Fail clearly on unsupported versions or invalid input. Do not convert incompatibility into ambiguous default behavior.

## Test Loop

1. Select the smallest test surface that expresses the user-visible behavior or invariant.
2. For a defect, prove the test fails for the reported reason before changing production code.
3. Implement the minimum fix and rerun the focused test until green.
4. Add boundary and negative cases revealed by the change; avoid duplicating equivalent assertions across layers.
5. Refactor names, structure, and duplication while keeping the focused suite green.
6. Run progressively broader checks: affected package, related domain packages, replay/contract/architecture tests, race tests for concurrency, then `make check`.
7. Review the final diff to ensure tests would detect a regression and no production-only escape path bypasses them.

Prefer deterministic state and value assertions. Assert errors by stable category, not full prose. Assert ordered lifecycle transitions when order is contractual. Keep fixtures minimal and name scenarios by behavior. If a required behavior cannot be tested at the current layer, improve the seam or explain the concrete blocker before handoff.

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

### Branches

- Keep `main` as the long-lived default branch. Do not make new work commits directly on `main`; create a short-lived task branch first unless the user explicitly requests another workflow.
- Use `<type>/<short-kebab-description>` when no task ID exists.
- Use `<type>/<TASK-ID-short-kebab-description>` when a task ID exists. Task IDs are optional, but never omit an available ID.
- Allow `feat`, `fix`, `hotfix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`, and `release` branch types.
- Use `release/v<major>.<minor>.<patch>` for a release branch.
- Write the description in lowercase ASCII kebab-case with a specific outcome. Keep it short enough to scan in logs.
- Keep one concern per branch. Do not encode author names, dates, environments, or implementation status in branch names.

Valid examples:

```text
feat/episode-outcome-tracking
fix/PIE-123-action-deadline
docs/git-conventions
release/v1.2.0
```

Invalid examples:

```text
feature_new_stuff
sx/changes
fix/2026-08-12
work/update
```

### Commits

Use Conventional Commits:

```text
<type>(<optional-scope>)!: <summary>
```

- Allow `feat`, `fix`, `refactor`, `perf`, `test`, `docs`, `build`, `ci`, `chore`, and `revert` commit types. Use `fix` for commits on `hotfix/*` branches.
- Use a lowercase domain or package name for the optional scope, such as `runtime`, `decision`, `behavior`, `contracts`, or `skill`.
- Write the summary in concise English, lowercase imperative mood, without a trailing period, and keep the complete subject at 72 characters or fewer.
- Explain why and important tradeoffs in the body when the subject is insufficient. Describe the change itself in the diff, not in a long procedural narrative.
- Mark a breaking change with `!` and add a `BREAKING CHANGE: <migration impact>` footer.
- Add `Refs: TASK-ID` when a task ID exists.
- Keep commits atomic: one coherent outcome, relevant implementation, tests, configuration, and documentation. Split unrelated changes before committing.
- Ensure every non-documentation commit builds and passes the risk-appropriate focused tests. Do not use a broken intermediate commit as a normal handoff.
- Do not leave `WIP`, `fixup!`, `squash!`, `update`, `changes`, `fix stuff`, or similarly vague messages in shared history.

Examples:

```text
feat(episode): track interaction outcomes
fix(runtime): enforce action cancellation deadline
docs(skill): define git conventions
feat(contracts)!: require protocol version

BREAKING CHANGE: adapters must send protocol_version in capability requests.
Refs: PIE-123
```

### Before Commit

1. Inspect `git status --short --branch` and confirm the branch name complies.
2. Preserve unrelated user changes and stage only files for the current outcome.
3. Inspect `git diff` and `git diff --cached`; check for secrets, generated-file edits, debug code, dead code, and accidental compatibility paths.
4. Run `git diff --check` and the risk-appropriate formatting, focused, full, race, architecture, static-analysis, and contract checks.
5. Confirm the staged change and commit message describe the same atomic outcome.
6. Never force-push, hard-reset, or rewrite user-owned commits without explicit approval.
