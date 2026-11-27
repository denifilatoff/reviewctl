# ReviewCTL implementation plan

The controller owns scheduling, durable identifiers, and publication. The agent evaluates code and findings.
Develop the four changes on separate branches from d5cc577. Integrate through ready-for-review pull requests.
Coordinate shared contracts before integrating the branches. Keep live checks on profiler PR 1075 read-only.

## 1. Paginate GitHub discussions

Follow cursors for both threads and comments, including individual thread reads during synchronization and recovery.
Keep existing snapshot, ownership, and readback checks. Reject partial results and cursors that fail to advance.
Test multiple pages at both levels and read all thread IDs on profiler PR 1075 without publishing a review.

## 2. Manage rounds, commands, and history

Keep a durable PR directory with separate rounds and passes. Save review.json, the reviewed SHA, finding evidence,
and publication outcome for each pass. Preserve the evidence that the report references.
Supply the whole PR history and an explicit previous report to the agent. Discard intermediate axis reports.
Every completed round publishes one short result, including a round on an already reviewed SHA.
Retrying delivery of the same round must recover its existing publication, including after a restart.

At the existing poll interval, process commands before scheduling work. Apply the PR author trust check to the
command author. A new text reply from the PR author in an open ReviewCTL thread triggers a full round even on
the same SHA. Coalesce pending replies and pushes. Ignore reactions and consume commands once.

- /reviewctl pause lets an active pass finish and saves its result. Hold publication and further passes.
- /reviewctl resume enables automation and processes accumulated work once, using the latest head.
  Resume an unfinished round when present. If only publication is pending, recheck its target and discussions.
  With no pending work, enable automation without starting a round.
- /reviewctl review requests one complete round even on the same SHA. Coalesce duplicate pending requests.
  During a pause it permits that round and its publication while leaving the PR paused afterwards.

Test same-SHA rounds, restart and retry, commands and pushes in one poll, and manual review during a pause.

## 3. Track findings and synchronize threads

Use the locked skill schema for the finding ID format. ReviewCTL allocates stable numbers per PR before the next pass
or publication. The agent identifies known defects and explains changes. Test compatibility on skill upgrades.
For existing PRs, assign IDs to open findings with links to their original threads.
Track finding status, verification SHA, GitHub thread identity and state, and who closed the thread where known.
Each new round reviews the full PR diff and all open findings. Revisit fixed findings when evidence suggests regression.

Close an owned thread when a fix was verified, recording the actual SHA even if the head moved.
On confirmed regression, reopen a controller-closed thread with evidence. Respect human closures and accepted risks.
Treat a manually resolved finding as declined. Omit it from later assessments, review summaries, and verdicts.
Keep full history in the registry. Publish changes requiring attention and avoid duplicating thread bodies.
Test stable numbering across passes, retries, migration, and both closure origins.

## 4. Catch up with pushes in the same round

Pin the first full pass. Before publication compare its SHA to the current head. If it moved, retain findings and
review the transition to the latest head in one pass, expanding to affected callers or the whole diff when needed.
Coalesce intermediate pushes. Allow five completed catch-up passes in addition to the initial pass.
At the limit, publish COMMENT against the last reviewed SHA, identify the observed head and distinguish the two.
Record thread fixes against their verified SHA. Once publication has begun, recover partial writes without duplicates
and treat subsequent feedback as a new round. Respect pause at pass boundaries.
Test A to B to D, repeated pushes through the fifth pass, pause, and a push during partial publication.

## Architecture and change boundaries

The references below describe commit `d5cc577`. The proposed handoffs describe what the branches must agree on.
Keep GitHub reads and writes in the controller. The model returns evidence and a proposed review.

### Discovery and scheduling (task 2)

[runCommandOnce](../cli.go) holds the run lock across discovery, a queue snapshot, and sequential calls to
`ProcessAttempt`. A second invocation returns `already_running`. A ten-minute timer alone therefore cannot observe
a pause during a long pass. Refresh commands at pass boundaries before starting another pass or sending a review.
A push and a pause observed together must save the pending head and hold delivery.

[shouldEnqueueDiscovery](../github.go) sees head changes, draft transitions, and new PRs after the initial baseline.
It receives no comment events. Collect commands from PR comments and review discussions, with author and comment ID.
Use the root comment and thread state to identify qualifying replies. Apply `isTrustedGitHubAuthor` to the command
sender. Process events in order, recording their consumption and pending work in the same SQLite transaction.
Extend [ApplyDiscoverySnapshot](../store.go) without replaying old commands or replies on the first upgraded poll.
The queue currently has one row per PR. Use it to wake the worker. Keep the paused flag, active round and pending
inputs separately so completing a round cannot delete a newer request.

### Controller state and pass handoff (tasks 2, 3 and 4)

| Data | Owner | Consumers |
| --- | --- | --- |
| PR pause state, pending input, round identity and manual request | Task 2 | Scheduler, catch-up, publication recovery |
| Ordered pass artifacts and previous report path | Task 2 | Agent prompt, findings registry, catch-up |
| PR finding ID, evidence, verification SHA, thread identity and closure origin | Task 3 | Next pass, discussion sync |
| Reviewed and observed SHAs, catch-up count and delivery mode | Task 4 | Pass scheduler, publication and readback |

[ProcessAttempt](../attempt.go) currently creates a temporary workspace, checks out one SHA, builds
`trustedInstruction`, runs Codex and reads a publication draft. The workspace is deleted on return.
Task 2 supplies a durable round and pass location. Task 4 selects full review or catch-up and supplies both
revision endpoints for catch-up. Task 3 supplies the finding context and validates the agent's finding references.
Compose those inputs at the existing instruction builder. Keep one model invocation per pass and one owner for
the decision to start another pass.

[ReviewDraft](../publication.go) is the controller's delivery payload. The skill's structured `review.json`
also has to survive the pass. Save the report and referenced evidence before temporary workspace cleanup.
Task 3 resolves pass-local findings to PR-wide IDs before task 4 can start the next pass. Use the durable
round/pass identity when retrying allocation. Stage artifacts first, then commit the completed pass pointer,
ID assignments and reviewed SHA together, or provide equivalent restart recovery. A crash between these steps
must neither expose an incomplete report as previous input nor allocate IDs twice.
Keep model usage from each executed pass and preserve existing delivery-only retry accounting.

### Discussion reads and writes (tasks 1 and 3)

Task 1 keeps the [list and single-thread read functions](../github.go) as the common source of complete threads.
Only this layer handles cursors. Task 2 uses thread metadata for scheduling. Task 3 consumes the same thread
identity, root author, replies and resolution state for migration and reassessment.
The existing ownership test uses the root author's login. It does not identify who resolved a thread.
Record controller closures after verified delivery. An unknown historical closure needs evidence before automatic
reopening. Keep the agent's finding status separate from GitHub's observed thread state.

[syncPublicationDiscussions](../discussion.go) rereads threads around mutations, recovers marked replies, and
verifies outcomes. Keep those checks when adding stable finding IDs. Link newly posted comments to findings after
GitHub assigns IDs. Reconcile an interrupted mutation from its saved intent and GitHub readback.

### Publication boundary (tasks 2 and 4)

[savePublication and deliverPublication](../publication.go) persist a payload per PR and search for a review marker
before posting. The marker currently includes PR, SHA and skill digest. Two rounds on one SHA would collide.
Task 2 owns the round-aware marker across creation, recovery, [signature delivery](../signature.go) and readback.
Retain support for pending payloads and signatures written with the old marker.

Task 4 owns the head policy at every call to `resolvePublicationTarget`, including discussion synchronization.
Before the first write, a newer head can send a saved pass back to catch-up. At the limit, keep the actual reviewed SHA
as the review's commit ID and publish COMMENT. Keep identity, trust, draft state, thread ownership and readback checks
in both delivery modes. A push after a write requires checking the remote marker and posted actions before resuming.
[Store.Finish](../store.go) currently deletes prepared payloads on `head_changed`; update this path alongside delivery
so it preserves the information needed to recover partial publication.

### Cross-branch acceptance tests

- Extend discovery and store fixtures for a pause and push in the same poll and a reply arriving during completion.
  After restart, resume consumes the pending inputs once.
- Exercise two full rounds on the same SHA and a retry after GitHub accepted the second POST but before local save.
  Expect two distinct reviews, stable finding IDs and no extra review or cost on retry.
- Use the publication recovery fixtures for a push during thread synchronization and for the fifth catch-up pass.
  Verify the COMMENT's commit, the fix verification SHA, closure origin and reply readback independently.
- Upgrade a populated SQLite fixture with an outstanding legacy publication. Recover it first, then run a new round
  on the same SHA, preserving both review identities and the old signature's readback.

## Integration

Ship pagination independently. Task 2 owns round identity, scheduling and publication markers. Task 3 owns the
finding registry and thread decisions. Task 4 consumes the round and finding contracts and owns pass transitions.
Run the Go suite on each branch and on the combined changes. Exercise restart and idempotency across task boundaries.
Use fixture tests for writes. Release and installation are separate from merging the implementation.
