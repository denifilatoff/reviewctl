# reviewctl operations

`reviewctl` queues GitHub pull requests locally and uses Codex during `run`. It stores configuration and state in your
user directories.

## Install

Install the native macOS or Linux release. The installer verifies the downloaded binary against `SHA256SUMS` and writes
`reviewctl` to `~/.local/bin` by default.

```shell
curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/scripts/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
reviewctl --help
```

Install authenticated [GitHub CLI](https://cli.github.com/), Codex CLI, and APM before running reviews.

## Initialize and configure

Create the configuration file and SQLite state database. `init` preserves an existing configuration file.

```shell
reviewctl init
```

The default configuration path is `~/.config/reviewctl/config.yaml`. Set `XDG_CONFIG_HOME` before a command to use
`$XDG_CONFIG_HOME/reviewctl/config.yaml` instead. The state database defaults to
`~/.local/state/reviewctl/reviewctl.db`; `XDG_STATE_HOME` changes that location.

Replace the initial configuration with your trusted authors and repositories. `publish: false` keeps `run` from
publishing reviews.

```yaml
harness: codex
model: gpt-6-astra
reasoning_effort: low
publish: false
attempt_timeout: 1h

trusted_authors:
  - <github-login>

repositories:
  - provider: github
    repository: <owner>/<repository-one>
  - provider: github
    repository: <owner>/<repository-two>
```

Only `github` and `codex` are valid values. `attempt_timeout` is optional, defaults to `1h`, and must be greater than
zero and no more than `24h`. `trusted_authors` applies to every configured repository. Only pull requests from those
GitHub logins may reach Codex.

## Model and review cost

Set the review model and reasoning effort with these settings:

```yaml
model: gpt-6-astra
reasoning_effort: low
```

ReviewCTL defaults to `gpt-6-astra` and `low` when either setting is omitted. Codex validates whether a selected model
supports the configured effort. History records both the requested settings and the model and effort observed in the
session logs.

Install [ccusage](https://ccusage.com/guide/) version 20.0.19 or newer within 20.x and put it on the same `PATH` as
`reviewctl`. Cost estimation uses `ccusage codex session --json --no-offline`, with only the review's session logs
and their native descendants copied into a temporary directory. No telemetry setup is needed. Codex runs are no
longer ephemeral: logs remain in the normal Codex home, which may contain review source and prompts. Temporary
accounting copies are removed after calculation. Do not remove active logs before the review finishes.

After the model finishes, the controller includes a signature as the last line of the initial GitHub review body:

```markdown
<sub>🤖 gpt-6-astra low in=12k cached=40k out=3k usd=0.083</sub>
```

The primary session supplies the model and reasoning effort. The token counts and estimated cost cover the review
run, including its native subagents. `in` excludes cache reads, `cached` counts cache reads, and `out` includes
reasoning tokens. The signature rounds token counts to the nearest thousand and adds `k`.
It rounds `usd` to two significant digits.
It omits unavailable fields and skips the signature when the observed model is unavailable. Local history
keeps the full estimate.

The cost estimates equivalent API usage. It does not represent a subscription charge or a billing statement.
Prices come from ccusage's catalog. Catalog lag and ccusage's pricing fallbacks can affect the estimate.

Missing ccusage, unreadable or incomplete logs, model-attribution fallbacks, warnings, or invalid reports produce
an unavailable estimate. The signature omits cost and unverified token counts, and review publication still succeeds.
Accounting has a separate 30-second timeout after Codex exits. `reviewctl --json status` exposes session IDs,
calculator version, calculation time, the report, and any accounting error. `doctor` does not check prices.

## Review publication and retries

Codex prepares the summary, inline findings, and actions for existing owned discussions. ReviewCTL validates that draft
for the pinned commit and saves it with its accounting before publishing. A `publication_failed` result retains the
prepared publication. The next `run` resumes delivery using the original draft and usage.

`draft_rejected` means GitHub returned a request validation error and readback confirmed that the review or discussion
reply was not created. ReviewCTL retires that plan and supplies the validation error to the next Codex attempt for
correction. An already-published review is preserved when its proposed discussion reply is rejected.
Setup failures leave this diagnostic available until the next Codex invocation. Later analyses do not receive it.
If readback is unavailable, or the matching reply already exists, the plan remains saved for marker recovery.

`review_dismissed` reports that GitHub dismissed the marked review for the current head. Inspect its dismissal reason
on GitHub. A newer PR head is assessed independently.

`review_conflict` means a reassessment produced new findings or a different verdict for a head that already has a
published review. The new draft and accounting remain saved; ReviewCTL keeps the PR queued and reports the existing
review URL. Repeated runs retain the conflict without invoking Codex or recording its original usage again.

Inspect the existing review and export the retained draft for manual assessment. For example, with SQLite's CLI:

```sh
sqlite3 -readonly "${XDG_STATE_HOME:-$HOME/.local/state}/reviewctl/reviewctl.db" \
  "SELECT json_extract(payload, '$.Draft') FROM review_publications \
   WHERE provider = 'github' AND repository = 'owner/repository' AND change_number = 123;"
```

Replace the repository and PR number with the reported target. Complete the assessment on GitHub as appropriate.
Automatic processing of this PR resumes when its head changes; other queued PRs continue normally. A new head retires
the obsolete draft after recording its accounting. Do not remove the saved publication to bypass this check.

ReviewCTL checks the current head before sending a new review. `head_changed` leaves the PR queued for its new revision.
If the head changes just after the check, GitHub attaches the review to the explicit commit, and the final check keeps
the PR queued. The submitted review already contains its accounting signature.

Review and reply markers let a retry recover a successful request whose response was lost. `discussion_changed`
retains the PR for reassessment when a human reply or state change invalidates a saved discussion action. Check the
reported PR in GitHub, then allow the next scheduled `run` to reassess it.

Discussion snapshots are checked before a new review is submitted. Before each discussion write, ReviewCTL rechecks
the pinned head and the affected thread. It reads a posted reply back before changing the thread's resolution, so
new human replies require reassessment rather than closure based on an older conversation.

Prepared publications in SQLite contain the review text, inline findings, and proposed replies until delivery completes.
Existing pending signature deliveries are retried separately. `signature_pending` returns exit code `1` until such
a delivery succeeds. An unavailable cost estimate is not automatically recalculated.

## Run in production on macOS

Keep `publish: false` while setting up a production schedule.

1. Save every repository and trusted pull request author in `~/.config/reviewctl/config.yaml`.
2. Run `reviewctl doctor` and resolve every failed prerequisite.
3. Run `reviewctl --json run` once. The first successful discovery records the open pull requests as a baseline and
   does not queue them.
4. Add any existing pull requests that still need review with `reviewctl review` or `reviewctl bulk-review`.
5. Change `publish` to `true`, run `reviewctl doctor` again, then run `reviewctl --json run` once manually.
6. Follow [Schedule with launchd](#schedule-with-launchd).

Later scheduled runs queue pull requests from trusted authors when they become ready, receive a new head, or become
eligible after a `trusted_authors` change.

## Check prerequisites

Run `doctor` after configuration changes and before enabling a schedule.

```shell
reviewctl doctor
```

It checks the configuration, authenticated GitHub access to every configured repository, Codex, locked APM skills,
SQLite state, and required local paths. Resolve every failed check before running reviews.

## Queue pull requests

`review` and `bulk-review` update only the local queue. They do not invoke Codex or publish a GitHub review.

```shell
reviewctl review https://github.com/<owner>/<repository>/pull/<number>
reviewctl bulk-review https://github.com/<owner>/<repository>/pull/<number> \
  https://github.com/<owner>/<repository>/pull/<number>
```

Duplicate pull requests report `already_queued` and remain safe no-ops. An invalid pull request URL returns exit code
`2`.

## Inspect state

`status` prints the pending queue, recent review history, and whether a `run` process holds the lock. It returns the
20 most recent history entries unless you set a limit.

```shell
reviewctl status
reviewctl status --limit 50
```

## Run one cycle

`run` polls configured repositories, updates the local queue, and processes one queue snapshot. It is the only command
that invokes Codex and may publish GitHub reviews when `publish: true`.

```shell
reviewctl run
```

Only one `run` process can execute at once. A concurrent call reports `already_running` and exits successfully. A
failed discovery or review attempt makes `run` exit with code `1`. Transient failures remain queued for a later run.
An untrusted, closed, or draft pull request leaves the queue and can return through discovery after its eligibility
changes. If a pull request head changes during an attempt, `run` reports `head_changed` and leaves the entry queued.

A review attempt can take several minutes, and `run` writes its result after the attempt finishes. Use
`reviewctl status` to check `run_active`; do not interrupt an active attempt or start a replacement process.

## Use JSON and exit codes

Place `--json` before the command when a script needs one result object on standard output.

```shell
reviewctl --json doctor
reviewctl --json status --limit 50
```

Exit code `0` means success or a safe no-op, such as `already_queued` or `already_running`. Exit code `1` means an
operational failure. Exit code `2` means invalid syntax or input. In JSON mode, both successful and failed results go
to standard output. In human mode, `doctor` and completed `run` commands report operational failures on standard
output. Errors that stop a command before it produces a result, such as invalid invocation or input and `run`
configuration, lock, or state setup errors, go to standard error.

## Schedule with launchd

On macOS, install a `launchd` job with a 10-minute interval:

```shell
(
  set -e
  installer=$(mktemp)
  trap 'rm -f "$installer"' EXIT
  curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/scripts/install-launchd.sh -o "$installer"
  sh "$installer" 600
)
```

The interval is optional and defaults to 300 seconds. The installer finds `reviewctl`, `gh`, `codex`, `apm`, and optional
`ccusage` in the current `PATH`, preserves the configured XDG locations, runs `reviewctl doctor` in the scheduled
environment, validates
the generated plist, and loads the job. It writes the plist under `~/Library/LaunchAgents` and logs under
`~/Library/Logs`. If a plist already exists, the installer asks before replacing it.

Inspect the loaded job and its logs with:

```shell
launchctl print "gui/$(id -u)/com.denifilatoff.reviewctl"
tail -n 100 "$HOME/Library/Logs/reviewctl.out.log"
tail -n 100 "$HOME/Library/Logs/reviewctl.err.log"
```

`state = not running` is expected between intervals because `reviewctl run` exits after one cycle. Confirm that
`runs` is greater than zero, `last exit code` is `0`, the latest standard-output record has `"status":"success"`,
and the standard-error log is empty.

Remove the scheduled job without deleting the binary, configuration, or SQLite state:

```shell
label=com.denifilatoff.reviewctl
plist="$HOME/Library/LaunchAgents/$label.plist"
launchctl bootout "gui/$(id -u)/$label"
rm -f "$plist"
```
