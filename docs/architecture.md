# reviewctl architecture

Status: Accepted MVP design

## Purpose

`reviewctl` schedules Codex reviews of GitHub pull requests on one trusted laptop. It polls configured repositories,
keeps a local queue, invokes Codex, and records the result in SQLite.

For installation, configuration, and scheduling, see the [operations guide](operations.md).

The tool uses the user's Codex subscription and GitHub CLI credentials. It does not require a hosted service, webhook,
GitHub App, GitHub Actions workflow, or separate API billing.

## Design constraints

- One `reviewctl run` process may execute work at a time.
- Other CLI processes may add pull requests or read status while `run` is active.
- GitHub is the only provider in the MVP.
- Codex is the only harness in the MVP.
- Pull request authors must appear in an explicit trust list before Codex may inspect or run their code.
- The implementation favors operating-system and SQLite behavior over custom coordination code.

The MVP leaves two source-level extension boundaries: one for another provider, such as GitLab, and one for another
harness. It does not implement either extension or a plugin system.

## Process model

`reviewctl` is a stateless CLI. Durable state lives in SQLite. An operating-system scheduler invokes `reviewctl run`
at a fixed interval.

On macOS, `launchd` owns the schedule and process restart behavior:

```text
launchd
   |
   | every configured interval
   v
reviewctl run
   |
   +-- discover GitHub changes
   +-- add pull requests to SQLite
   +-- process one queue snapshot
   +-- exit
```

`run` takes one process lock for its entire lifetime. A second `run` reports `already running` and exits without
changing state. Producer and reader commands do not take this lock.

The lock prevents two consumers from executing the same queue. It does not protect SQLite. SQLite transactions
coordinate concurrent queue inserts, history writes, and status reads.

`reviewctl` does not hold a SQLite transaction while it calls GitHub, installs skills, or runs Codex.

## Components

```mermaid
flowchart LR
    Timer["launchd timer"] --> Run["reviewctl run"]
    User["User or agent"] --> CLI["review / bulk-review / status"]
    Run <--> DB["SQLite"]
    CLI <--> DB
    Run --> GH["GitHub through gh"]
    Run --> APM["APM"]
    Run --> Codex["Codex"]
    Codex -->|read context| GH
```

| Component | Responsibility |
| --- | --- |
| CLI | Parse commands, validate local input, and perform short SQLite operations |
| Run cycle | Discover changes and process one queue snapshot sequentially |
| GitHub code | Resolve PR state, publish reviews, synchronize owned discussions, and verify readback |
| Codex code | Start and cancel Codex, then decode the prepared review draft |
| APM | Install the locked review skills into a fresh attempt workspace |
| SQLite | Store discovery baselines, queued PRs, prepared publications, and review history |

## Configuration

Configuration lives at `$XDG_CONFIG_HOME/reviewctl/config.yaml`, or `~/.config/reviewctl/config.yaml` when
`XDG_CONFIG_HOME` is unset.

```yaml
harness: codex
model: gpt-6-astra
reasoning_effort: low
publish: true
attempt_timeout: 1h

trusted_authors:
  - alice
  - dependabot[bot]

repositories:
  - provider: github
    repository: acme/service-a
  - provider: github
    repository: acme/service-b
```

The MVP accepts only `github` and `codex`. The names keep the two agreed extension boundaries visible without adding
runtime registries, capability negotiation, or plugin loading.

`trusted_authors` must contain at least one exact GitHub login. Comparison is case-insensitive after normalization. An
empty list blocks discovery and execution.

Automatic publication requires `publish: true`. ReviewCTL checks this permission before invoking Codex and before
publishing. The agent receives a read-only review task. GitHub and Codex credentials remain owned by their CLIs.

`attempt_timeout` bounds one complete review attempt. It defaults to one hour and accepts a positive Go duration up to
24 hours. Timeout or cancellation terminates the Codex process group, records a stable failure, and leaves the pull
request queued.

The implementation decodes YAML directly. It does not use a configuration precedence framework.

## Discovery baseline

The baseline records the last observed state of each open pull request from a trusted author in each configured
repository. It is not a job queue or execution history.

The first successful poll of a repository establishes its baseline:

- Existing eligible pull requests are recorded without being queued.
- Existing drafts from trusted authors are recorded so a later ready transition can be detected.

Later polls enqueue a pull request when:

- a new pull request is ready for review;
- a draft becomes ready; or
- the head SHA of an eligible pull request changes.

The fetched GitHub snapshot is obtained outside a database transaction. Queue inserts and the corresponding baseline
update use one short SQLite transaction, so a crash cannot advance the baseline without recording the work.

Discovery requests each pull request's author and a 1,001-item sentinel through `gh`. Pull requests from untrusted
authors do not enter the baseline or queue. A repository with more than 1,000 open pull requests fails discovery
without changing its baseline or queue; supporting that repository requires pagination.

A skill change does not enqueue every open pull request. Adding an author to `trusted_authors` makes that author's
ready pull requests eligible on the next poll. The user may also enqueue selected pull requests explicitly.

## Review queue

The queue is the durable set of pull requests that still need a review. A queue entry identifies only the pull
request:

```text
provider + repository + provider-local change number
```

The MVP uses `github` and a pull request number. The provider field allows a later GitLab implementation to use a merge
request IID without changing the queue model.

The queue does not store leases, owners, worker generations, retry deadlines, job states, skill digests, receipts, or
workspace paths. Duplicate events use `INSERT OR IGNORE` against the pull request identity.

The service resolves the current head SHA when it starts a review. If the head changes while a pull request waits, only
the latest revision needs review.

One `run` invocation reads a queue snapshot and processes every entry in that snapshot at most once, in deterministic
insertion order. Entries added after the snapshot wait for the next invocation.

`run` records history and updates the queue in one transaction. A confirmed success or an eligibility rejection
(`untrusted_author`, `pr_not_open`, or `pr_draft`) removes the pull request. Other failures leave it queued. Discovery
can queue the pull request again after its author becomes trusted, it is reopened, or its draft status changes. The
operating-system schedule supplies the retry cadence; the MVP has no retry loop, backoff policy, or retry counter.

ReviewCTL saves a prepared publication in the `review_publications` table before any GitHub write. The row contains
the pinned attempt, original accounting, final review body, inline comments, reviewer login, and initial discussion
snapshot. A retry loads this row and checks GitHub for the review marker before sending a request. The original
accounting is recorded once, including when the process exits after publication and before the history transaction.
Successful delivery clears the prepared publication in the history transaction.

Known revision drift, an eligibility rejection, a stale discussion action, or a confirmed GitHub draft rejection also
retires the prepared publication after recording its original accounting. Uncertain request failures retain it for
readback and retry.

## History

Review history is separate from the queue and never controls scheduling. Each processing attempt records a bounded row
with:

- pull request identity and reviewed head SHA;
- start and finish times;
- success or failure;
- review verdict when available;
- published review identifier and URL when available; and
- a bounded error code and message on failure.

History stores review identifiers and accounting. The separate prepared-publication row retains review text and
discussion replies until delivery completes. These drafts may contain source excerpts and other review context.

History is append-only for normal operation. Retention and telemetry export are outside the MVP.

## Producer commands

Producer commands add work and exit after SQLite confirms the transaction:

```text
reviewctl review <pull-request-url>
reviewctl bulk-review <pull-request-url>...
```

`review` accepts exactly one pull request. Automatic discovery calls the same enqueue application function; it does
not start a nested CLI process.

`bulk-review` is a manual convenience for a user or agent. It validates the list, adds the pull requests in one short
transaction, and reports which entries were added or already present. It does not execute Codex or create a separate
batch scheduler.

Both commands may run while `run` is active. SQLite may briefly wait for another SQLite transaction, but neither
command waits for a review to finish.

## Run cycle

`reviewctl run` performs one cycle and exits:

1. Acquire the process lock.
2. Load and validate configuration.
3. Poll configured GitHub repositories and update the baseline and queue.
4. Read one deterministic snapshot of the queue.
5. Process each snapshot entry once, sequentially.
6. Release the process lock and exit.

Parallel Codex sessions are outside the MVP. If sequential execution becomes too slow, one `run` process may later
divide its snapshot among a bounded in-memory worker pool. The queue schema does not need leases or worker ownership
for that change.

## Review attempt

One attempt is one Codex execution for one queued pull request. The attempt resolves and pins these values before Codex
starts:

```text
provider + repository + pull request number + head SHA + installed skill digest
```

The execution path is linear:

1. Resolve the configured pull request and current head through `gh`.
2. Verify that the repository is configured and the exact author login is trusted.
3. Create a fresh temporary workspace.
4. Install and hash the locked skill set with APM.
5. Write one trusted instruction.
6. Run Codex to prepare a draft for the pinned SHA, including proposed actions for owned discussions.
7. Validate the draft, add the marker and available accounting signature, and save the prepared publication.
8. Recheck eligibility and the head, publish through `gh`, verify readback, and synchronize owned discussions.
9. Remove the workspace, record history, and remove the queue entry after confirmed success or an eligibility rejection.

A saved publication resumes before another Codex invocation. An existing marked review is recovered after verifying
its author, commit, URL, and submitted event. Owned discussions can be reassessed by Codex when another delivery
attempt has no saved plan.

A reassessment retains its own draft. If it reports independent new findings or changes the verdict of an existing
review, `review_conflict` preserves that draft and keeps the PR queued for operator assessment. Inline comments always
count as new findings on this path; the optional draft field `new_findings` also identifies general findings in the
summary and defaults to false for older saved drafts. Recovery does not replace a new assessment with an old approval.

`APPROVE` and `REQUEST_CHANGES` are valid when the authenticated reviewer did not author the pull request. `COMMENT` is
valid and required when GitHub forbids a decisive review because the authenticated reviewer authored the pull request.
These outcomes are not process failures.

If the head changes before the publication check, ReviewCTL returns `head_changed` and keeps the PR queued.
GitHub review creation includes the pinned `commit_id`. A change after the check leaves the review attached to that
explicit commit with its accounting signature. The final head check keeps the new revision queued.

## Trust and publication

Only pull requests from configured authors are eligible. `reviewctl` checks the exact normalized GitHub login before
Codex starts. An untrusted pull request is never cloned, executed, or published. The MVP has no bypass flag.

The trusted instruction limits Codex to one repository, pull request, expected head SHA, installed skill set, and
publication policy. Repository instructions may guide review and test commands, but they cannot broaden that scope.

Codex and the locked review skill produce a structured draft containing the pinned scope, verdict, summary, inline
comments, and one proposed action for each owned discussion. The instruction forbids GitHub writes and switching
the attempt to another SHA. ReviewCTL validates the draft before preparing an external request.

ReviewCTL owns review creation and discussion writes. Its initial review request includes the exact `commit_id`,
idempotency marker, and available accounting signature. Readback verifies the reviewer, commit, URL, event, body, and
inline comments. A saved publication requires the same authenticated reviewer on retry.

Before changing a discussion, ReviewCTL verifies that the authenticated reviewer authored its root comment and that
pre-existing comments remain intact. Each reply contains a marker tied to the prepared publication and thread. A retry
recovers that reply before attempting another write. Resolve and reopen operations follow the proposed action and are
verified against the complete discussion snapshot. `discussion_changed` keeps the PR queued for reassessment when
human comments or state changes invalidate a saved action.

Before posting a new review, ReviewCTL validates the discussion snapshot used for its assessment. Discussion writes
use fresh reads of the affected thread and current head. After posting a reply, the controller reads it back and
checks for intervening human activity before resolving or reopening. Preserved actions share full snapshot reads;
mutating actions read only their own thread before writing, with a full final readback of all outcomes.

The controller resolves the authenticated GitHub login directly through `gh`. For a self-authored PR it submits
`COMMENT` while retaining the assessment in the review body. Other PRs receive `APPROVE` or `REQUEST_CHANGES`.

The trust policy assumes one trusted user account on one trusted laptop. It does not defend against another process
running as that user or prove who authored every commit in a trusted pull request.

## Skills and APM

The source checkout contains one `apm.yml` and one `apm.lock.yaml`. The manifest may list several skills required for
review. The lockfile pins their exact versions.

Each attempt runs:

```shell
apm install --frozen --root <attempt-workspace> --target codex
```

`reviewctl` hashes the installed skill set once and includes the digest in the instruction, marker, draft validation,
and history. An APM failure stops the attempt before Codex starts and is recorded in history.

`reviewctl` does not implement skill lookup, copying, snapshot recovery, or configuration precedence. Updating review
skills means updating the checked-in APM lockfile.

## Workspace

Each attempt uses a new temporary directory. The workspace contains the APM installation, trusted instruction, draft
path, and source checkout. The implementation uses ordinary temporary-directory creation and recursive removal.

The workspace is deleted after success or failure. A later retry creates a new workspace. SQLite retains history and unfinished publications with their original accounting. Review text is removed from the
prepared-publication table after delivery completes.

The MVP does not recover, reuse, fence, reference-count, or selectively clean workspaces. It does not implement custom
filesystem capabilities or defend against the trusted local user replacing files during execution.

## Provider and harness boundaries

GitHub-specific calls live together, and Codex-specific process handling lives together. Application code calls these
two narrow boundaries without a runtime plugin registry.

A GitLab implementation may later add provider-specific discovery and merge request handling. Another harness may
later add its own command and draft decoding. Neither future implementation is part of the MVP, and the MVP does not
normalize features that GitHub and Codex do not need.

## CLI surface

```text
reviewctl [--json] init
reviewctl [--json] doctor
reviewctl [--json] review <pull-request-url>
reviewctl [--json] bulk-review <pull-request-url>...
reviewctl [--json] run
reviewctl [--json] status [--limit <count>]
reviewctl --help
reviewctl --version
```

- `init` creates configuration without overwriting an existing file.
- `doctor` checks configuration, GitHub, Codex, APM, the locked skill set, SQLite, and local paths.
- `review` enqueues one pull request.
- `bulk-review` enqueues several pull requests in one transaction.
- `run` performs one discovery and processing cycle.
- `status` shows the pending queue, bounded recent history, and whether a `run` lock is held. Its default history limit
  is 20.

Commands do not open an editor, pager, or interactive confirmation. They do not depend on the source checkout or the
caller's current directory.

## Agent-friendly CLI contract

Human-readable text is the default. The global `--json` flag gives every operational command one deterministic,
machine-readable result. JSON mode writes exactly one JSON object to stdout on success or failure. Stderr is reserved
for diagnostics and never contains result data. The MVP emits no color or progress animation.

The process exit code has only three meanings:

- `0`: success or a safe idempotent no-op, including `already_queued` and `already_running`;
- `1`: an operational failure, including a `run` in which any queued review failed; and
- `2`: invalid command syntax or input.

JSON failures contain a short stable error `code` and a bounded message. The MVP does not add report versions, JSON
Schema files, or a separate agent protocol.

Command results expose the smallest useful facts:

- `init` returns the configuration path it created.
- `doctor` returns each prerequisite and whether it is ready; the APM check also returns the installed skill digest.
- `review` returns the pull request identity and `queued` or `already_queued`.
- `bulk-review` returns one result per argument in input order.
- `run` returns discovery and processing counts plus the outcome and review URL for each attempted pull request.
- `status` returns the queue, at most the requested number of history rows, and whether `run` is active.

Help text states that `review` and `bulk-review` change only the local queue, while `run` may publish GitHub reviews
when publication is enabled. This keeps the external-write boundary visible to a calling agent without adding an
interactive confirmation.

## Local persistence

Default paths follow XDG conventions:

```text
config: $XDG_CONFIG_HOME/reviewctl/config.yaml
state:  $XDG_STATE_HOME/reviewctl/reviewctl.db
cache:  $XDG_CACHE_HOME/reviewctl/
lock:   $XDG_RUNTIME_DIR/reviewctl/run.lock
```

When an XDG variable is unset, `reviewctl` uses the corresponding user directory under `~/.config`, `~/.local/state`,
or `~/.cache`. The lock falls back to the state directory when no runtime directory is available.

## Consistency rules

- Only one `run` process consumes the queue.
- Producer and status commands remain available while `run` is active.
- The queue contains one entry per pull request.
- A queue entry is deleted after confirmed success, recovery of an existing published review, or an eligibility
  rejection.
- Every failed Codex execution writes bounded history and leaves the pull request queued.
- Discovery updates the baseline and queue atomically.
- No database transaction remains open during external commands.
- No untrusted pull request reaches Codex.
- Publication requires explicit configuration and an exact head check.
- GitHub review outcomes remain separate from process success or failure.

## Tests

Unit tests cover pure configuration, URL parsing, trust checks, event detection, marker and receipt validation, and
queue decisions.

Integration tests use real SQLite and temporary directories. Scripted `gh`, APM, and Codex processes verify one full
cycle, concurrent producers, process-lock behavior, failure retention, success deletion, history, cancellation, and
marker recovery. Tests do not reproduce a multiprocess lease or filesystem attack matrix.

Black-box CLI tests invoke the built command from a temporary directory without a TTY. They parse every `--json`
result, verify stdout and stderr separation, exit codes, deterministic bulk ordering, bounded status output, and the
documented idempotent no-op results.

A local live end-to-end test uses the real GitHub and Codex credentials only on the
[canonical fixture PR](https://github.com/denifilatoff/reviewctl/pull/24). It rechecks the repository, pull request,
author, open non-draft state, head, and exact marker count before Codex. It accepts zero or one marker and rejects
duplicates. Every invocation establishes the discovery baseline and records one publication-disabled retryable
failure. With zero markers, it verifies a published `COMMENT` review; with one marker, it verifies recovery. Both modes
enqueue the fixture again and verify another recovery without duplicate publication. Missing live prerequisites block
this test rather than turning it into a passing skip. The first zero-marker run proves publication; later runs prove
recovery without requiring a fixture head change.

## Non-goals

- Multiple concurrent `run` consumers.
- Parallel Codex sessions in the MVP.
- Renewable leases, worker ownership, retry deadlines, or crash reclaim.
- A long-running daemon, IPC protocol, or HTTP control server.
- Runtime provider or harness plugins.
- GitLab or a harness other than Codex.
- Workspace reuse or recovery.
- A custom secure filesystem layer.
- Storing complete diffs or source trees in SQLite.
- A service installer, self-update, or Windows support. Packaged binary releases support macOS and Linux.
