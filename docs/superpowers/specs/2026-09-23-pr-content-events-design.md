# PR Content Event Reviews

## Context

ReviewCTL discovery currently queues an eligible pull request when it first appears, becomes ready, or changes head SHA.
It does not queue another attempt when the pull request description changes or a person adds a message without pushing a
commit.

A recent manual ReviewCTL task exposed a separate instruction problem. The approval layer treated the repository rule
for the canonical live E2E fixture as a ban on normal `reviewctl run` publication. The process never started, even
though the trusted attempt prompt and the pinned review skill already authorize review publication and
owned-discussion updates.

## Goals

- Queue a new attempt when an eligible pull request description changes.
- Queue a new attempt for every new non-empty human-authored PR message.
- Coalesce changes observed in the same discovery cycle into one attempt.
- Prevent ReviewCTL's own publication from triggering another attempt.
- Preserve idempotent recovery and detect input changes that happen during an attempt.
- Clarify the live E2E instruction without broadening ReviewCTL's publication authority.

## Non-goals

- Webhook delivery.
- Reprocessing edits to existing comments or deleted comments.
- Reacting to labels, approvals without text, requested reviewers, checks, or other PR metadata.
- Changing which pull requests are eligible or which discussions ReviewCTL may modify.
- Adding a single-PR `run` command.

## Instruction chain

The repository `AGENTS.md` will state that the canonical fixture restriction applies only to `make live-e2e` and
`scripts/live-e2e.sh`. A configured production `reviewctl run` may publish reviews and synchronize owned discussions
only as authorized by its trusted prompt and pinned skill.

The pinned skill needs no semantic change. Its contract already separates publication from discussion ownership:

- publication is authorized when the caller explicitly requests it;
- replies and resolve/reopen actions are allowed only for discussions owned by the publishing account;
- readback is required after mutation.

This clarification does not make an ad hoc request for one PR authorize unrelated queued PRs. Manual callers must not
start the whole queue unless that queue is already authorized; the configured service remains the normal queue
processor.

## Chosen design

### External input revision

For each eligible PR, discovery computes a deterministic external input revision from:

- the exact PR description;
- the accumulated stable IDs of every non-empty human-authored issue comment;
- the accumulated stable IDs of every non-empty human-authored review body;
- the accumulated stable IDs of every non-empty human-authored inline review comment or reply.

The publishing account's messages and GitHub bot accounts are excluded. All other people count, including the PR author
and reviewers. The baseline keeps the union of previously and currently observed IDs. Comment edits and deletions do not
change the revision; a new message adds an ID.

The existing baseline gains the description digest, observed message IDs, and external input revision. On first
observation, discovery records them without queueing an existing unchanged PR, preserving current startup behavior. A
later revision change queues the PR and advances the baseline.

### Polling

The existing repository-level PR list remains the eligibility query. Discovery then makes one GraphQL detail request for
each eligible, ready PR. That request returns the description, authenticated viewer, and the first page of all four
message channels. ReviewCTL follows every non-empty connection cursor before accepting the snapshot.

GitHub does not document a pull request's `updatedAt` as a complete signal for child review comments and replies, so it
cannot safely suppress detail reads. Metadata-only activity cannot queue an attempt because it does not affect the
external input revision.

This is intentionally polling, not an event log: a message created and deleted entirely between polls cannot be
observed.

### Queue and idempotency

The queue entry gains a nullable external input revision:

- discovery-created entries carry the revision that triggered them;
- repeated discovery updates replace it with the latest revision, coalescing several events before processing;
- existing and manually queued entries may leave it empty and retain the current head-based marker behavior.

When present, the revision is appended to the existing review marker and is included in the trusted prompt, receipt, and
attempt history. The marker remains the idempotency key for retries of the same head, review instructions, and external
input. Keeping the new field nullable preserves recovery of existing legacy markers and avoids a migration-only
duplicate review.

### Changes during an attempt

An event-triggered attempt resolves the current external input before analysis. Immediately before accepting the receipt
as successful, ReviewCTL reads it again. If a person changed the description or added a message during the attempt,
ReviewCTL records an input-change failure and leaves the PR queued with the newest revision.

ReviewCTL's own review, replies, and discussion state changes are ignored by the revision, so publication cannot create
a self-sustaining loop.

## Rejected alternatives

- Use `updatedAt` as the event identity or candidate gate: rejected because GitHub does not define it as a complete
  child-message signal, while ReviewCTL's own writes and unrelated metadata can also update it.
- Nest every conversation under the repository list query: rejected because large repositories can exceed GraphQL node
  limits and nested connections cannot be paginated safely as one result.
- Add webhooks: rejected because they require new deployment, authentication, retry, and persistence infrastructure for
  a polling service that already has the needed schedule.

## Storage changes

The existing SQLite migrations add:

- `description_digest`, `message_ids_json`, and `input_revision` to discovery baselines;
- nullable `input_revision` to queued items and attempt history.

No new table or dependency is needed. Existing rows remain valid and are initialized on their next discovery
observation.

## Failure handling

- A detail-query failure does not advance that PR's baseline, so the next poll retries it.
- Pagination must complete before a revision is accepted; partial conversations are never hashed.
- A missing author is treated as non-human and ignored.
- If the publishing account cannot be resolved, discovery fails rather than risk a feedback loop.
- Queue insertion and baseline advancement remain in one transaction.

## Verification

Focused tests will cover:

- description changes and each supported human message channel queue an attempt;
- bot, publishing-account, empty-body, comment-edit, and metadata-only changes do not queue;
- multiple changes before processing coalesce to the latest revision;
- legacy empty-revision markers still recover;
- same-head events produce distinct markers, while retries reuse one marker;
- an input change during an attempt leaves the PR queued;
- GraphQL pagination produces a complete deterministic revision.

The full Go test suite must remain green. Live publication, if needed, uses only the documented canonical fixture PR.

## Expected API cost

Each poll adds one GitHub GraphQL detail request per eligible, ready PR, plus one request for each extra conversation
page. With a ten-minute schedule, `N` continuously open eligible PRs add about `144 × N` requests per day before
pagination. The number of Codex review runs increases by the number of poll windows that contain at least one qualifying
description change or new human message, not by the raw number of messages in that window.
