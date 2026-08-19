# Architecture decision records

This directory stores reviewed architecture decision records (ADRs). The accepted MVP design remains in
[`docs/architecture.md`](../architecture.md); do not turn every design section into an ADR.

## ADR candidates

Review ADR candidates after the initial implementation and after later changes that make a durable architecture
choice. A candidate should affect system structure, a non-functional requirement, a dependency, a published
interface, or a construction technique.

Use all available implementation evidence:

- the delivered code, tests, and documentation;
- relevant GitHub issues, pull requests, and review discussions;
- ignored Superpowers specs and plans under `docs/superpowers/` or `.superpowers/`;
- the available user-agent conversation that led to the implementation.

Do not commit raw Superpowers documents or conversation transcripts. Do not reconstruct unavailable conversation
content. Extract only the decision, its context, the alternatives considered, its consequences, and links to durable
repository or GitHub evidence.

Before closing an implementation issue, post its candidate list to that issue for the repository owner's approval.
For each candidate, include a proposed title, decision, reason, alternatives, consequences, and evidence. State
`ADR candidates: none` when the review finds no qualifying decision. The final initial-implementation task aggregates
unresolved candidates on the implementation epic.

Create an ADR only after the repository owner approves its candidate. A new ADR starts as `Proposed`; change it to
`Accepted` only after explicit approval.

## File contract

- Store one decision per file.
- Use a four-digit sequence and a lowercase, dash-separated name, such as `0001-job-identity.md`.
- Use one of these statuses: `Proposed`, `Accepted`, `Rejected`, or `Superseded`.
- Add the acceptance date only when the status becomes `Accepted`.
- Treat an accepted ADR as immutable. Replace it with a new ADR and cross-link both records when the decision changes.
