# ADR 0001: Generate Architecture Decisions As Result Docs

## Status

Accepted

## Context

Docs Seed generates human reading documents from code, Git history, and branch ancestry. The initial result set contained only business logic and data flow pages. That was enough to explain behavior, but it left durable architecture choices implicit in source layout, configuration boundaries, data ownership, and orchestration flows.

Readers need a stable place to review those decisions without treating implementation details as the documentation contract.

## Decision

Each branch result directory includes an ADR result document alongside business logic and data flow:

- `business-logic.md` records business rules, state changes, orchestration, and failure or compensation behavior.
- `data-flow.md` records where data enters, how it is transformed or persisted, and where it exits.
- `adr.md` records architecture decisions already evidenced by the source tree, configuration boundaries, data ownership, or workflow composition.

The learned fact schema includes `architecture_decisions`. ADR entries must be evidence-backed and must not invent future decisions, implementation instructions, API examples, command usage, or setup steps.

## Consequences

Docs Seed output now separates behavior, data movement, and architecture decisions. Existing generated documents remain compatible because missing `architecture_decisions` fields render as an empty ADR page until the branch is relearned.

Prompt and validation rules must keep ADR content inside the human reading boundary: decisions and consequences are allowed; code invocation guidance is not.
