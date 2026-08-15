# ADR 0005: Route Language with Deterministic Policy

- Status: Accepted
- Date: 2026-08-15

## Context

The product needs reviewed social phrases, bounded local generation, and later
opt-in remote reasoning without turning an LLM into the interaction controller.
A short request can require current external data, while a complex request can
contain device-private memory. Text complexity alone is therefore neither a
privacy policy nor a reliable routing signal.

The current hands-free reply path records only that speech occurred. It does
not carry a transcript or dialogue content into the Engine. Language routing
must remain outside that canonical Observation and semantic-audit path.

## Decision

Use one versioned, pure routing policy over application-classified facts. The
router consumes a typed task, privacy class, priority, explicit per-request
cloud authorization, remaining deadline and token bounds plus an immutable
runtime snapshot. It never reads raw text, invokes a model, reads a clock, or
authorizes an action.

The execution targets are runtime backends, not sequential product stages:

- `TEMPLATE` realizes reviewed system, safety, permission, degradation and
  clarification text.
- `LOCAL` is reserved for bounded casual or device-private realization on an
  explicitly available GPU.
- `CLOUD` is reserved for public external-knowledge requests after both global
  enablement and explicit authorization for the individual request.
- `NONE` means that no language work may run.

P0, silence and cancellation route to `NONE`. Physical-action and permission
requests route only to reviewed explanatory templates; a model cannot approve
or execute them. Device-private and sensitive requests never route to cloud.

Cloud eligibility requires every gate to pass: enabled policy, explicit
request authorization, `PUBLIC` privacy, network availability, backend health,
closed circuit, available capacity, sufficient remaining deadline, token
limits and both session and daily token budgets. Local eligibility similarly
requires enabled policy, GPU availability, health, closed circuit, capacity,
deadline and token limits.

Each plan contains an ordered fallback graph. Casual local generation may only
fall back to templates and can never escalate to cloud after a local failure.
An external-knowledge cloud request falls back only to a clarification or
unavailable template; a local model must not fabricate current external facts.
Executors may move only left-to-right through the declared plan.

Load only strict `schema_version: v1` configuration. Reject missing and unknown
fields, alternate versions, implicit type coercion and multiple YAML documents.
The canonical SHA-256 hash includes every configurable semantic field in
normalized units.

Keep this first slice pure, uncomposed and template-only in the checked-in v1
configuration. A later reviewed slice may add a typed dialogue boundary,
isolated local worker and outbound HTTPS adapter. It must not place transcripts
in Engine observations, add inbound network listeners, or let model output
bypass deterministic policy and action validation.

## Consequences

- Routing decisions are replayable from policy, request and runtime snapshot.
- Remote processing cannot be enabled by backend failure or ambiguous input.
- Model, GPU and network outages retain a bounded local template path.
- Backend executors, circuit-breaker mutation, queues, prompt construction and
  output validation remain future adapter/application orchestration work.
- The template backend remains mandatory; local and cloud are explicit policy
  options rather than required availability.

## Rejected Alternatives

- A learned routing network in v1: there is no labeled routing corpus, and a
  misroute could disclose private data. A future local classifier may provide
  a shadow-mode suggestion but can never override hard gates.
- Route by simple-versus-complex text: it ignores freshness, privacy, safety,
  latency and cost.
- Automatically try cloud after local failure: operational failure does not
  grant remote-processing consent.
- A language microservice, message bus, or LLM controller: none is required for
  this pure policy slice and each would weaken current boundaries.
