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

After the model finishes, the controller adds one line to the GitHub review, for example
`Model: gpt-6-astra low (~$0.1)`. Model and reasoning effort come from the primary agent's session.
The displayed amount is rounded to one decimal place; history keeps full precision.
The amount includes native subagents, using ccusage's token and inherited-history accounting. It is an
API-equivalent estimate, not a subscription charge or a billing statement. Internet prices come from ccusage's
upstream catalog; reviewctl does not maintain a pricing table. Catalog lag and ccusage's pricing fallbacks can
affect the estimate.

Missing ccusage, unreadable or incomplete logs, model-attribution fallbacks, warnings, or invalid reports produce
`cost unavailable`, not a guessed total. Review publication still succeeds. Accounting has a separate 30-second
timeout after Codex exits. `reviewctl --json status` exposes session IDs, calculator version, calculation time,
the report, and any accounting error; `doctor` does not validate pricing availability.

Failed signature delivery is saved in SQLite and retried by the next `run`, without repeating the review.
`run` reports `signature_pending` and exits with code `1` until delivery succeeds. An unavailable estimate is
not automatically recalculated. Recovery of an existing review does not replace its original model or cost.

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
