# reviewctl

`reviewctl` queues GitHub pull requests on one trusted laptop and asks Codex to publish reviews. It discovers review
events in configured repositories and also accepts explicit pull request URLs.

## Build and install

You need Go 1.25, `gh`, Codex CLI, and APM. Authenticate `gh` and Codex before running `reviewctl`.

Run the repository gates, build the binary, and install it on your `PATH`:

```shell
make ci
go build -o reviewctl .
mkdir -p "$HOME/.local/bin"
install -m 0755 reviewctl "$HOME/.local/bin/reviewctl"
```

The binary embeds `apm.yml` and `apm.lock.yaml`. Every review attempt installs the pinned review skill into a fresh
workspace with `apm install --frozen`; it does not read an APM lock from the caller's working directory. Update and
commit both APM files together when changing the review skill.

## Initialize and configure

Create the XDG configuration and SQLite state paths:

```shell
reviewctl --json init
```

`init` creates a fail-closed starter configuration and the state database. A later `init` preserves the existing
configuration byte for byte.

Edit `$XDG_CONFIG_HOME/reviewctl/config.yaml`. When the XDG variable is unset, the path is
`~/.config/reviewctl/config.yaml`.

```yaml
harness: codex
publish: true
attempt_timeout: 1h
trusted_authors:
  - dependabot[bot]
repositories:
  - provider: github
    repository: owner/repository
```

The MVP accepts only `harness: codex` and `provider: github`. `run` rejects repositories outside the configured list
and authors outside `trusted_authors`. It stops before APM, checkout, or Codex when `publish` is false.

`attempt_timeout` defaults to one hour. It accepts a positive Go duration up to 24 hours.

## Queue and run reviews

Queue one pull request without invoking Codex:

```shell
reviewctl --json review https://github.com/owner/repository/pull/123
```

Queue several pull requests in one transaction:

```shell
reviewctl --json bulk-review \
  https://github.com/owner/repository/pull/123 \
  https://github.com/owner/repository/pull/124
```

The results stay in argument order. A new identity reports `queued`; a duplicate reports `already_queued` and exits
successfully.

Run one discovery and processing cycle:

```shell
reviewctl --json run
```

The first successful discovery of a repository records its open pull requests without queueing them. Later cycles
queue a new ready pull request, a draft that becomes ready, or a changed head on a ready pull request. Each `run` reads
one queue snapshot, attempts it sequentially, and exits. Items added after the snapshot wait for the next cycle.

Inspect the queue, the newest 20 history rows, and the consumer lock:

```shell
reviewctl --json status
reviewctl --json status --limit 5
```

`status` returns `run_active`, the queue in insertion order, and at most the requested number of history rows in
newest-first order. A competing `run` returns `already_running` with exit code 0. Queue producers and `status` remain
available while the active run owns the machine-wide lock.

State lives at `$XDG_STATE_HOME/reviewctl/reviewctl.db`, or `~/.local/state/reviewctl/reviewctl.db` when the XDG
variable is unset.

## Results, failures, and retries

The global `--json` flag writes exactly one result object to stdout. JSON mode keeps stderr empty. Human-mode results
use stdout, while invalid syntax and input use stderr.

Exit codes have three meanings:

- `0`: success or a safe idempotent no-op, including `already_queued` and `already_running`;
- `1`: an operational failure, including discovery or review failure; and
- `2`: invalid syntax or input.

A failed attempt appends a bounded history row and leaves the pull request queued. `reviewctl` has no internal retry
loop or backoff. The next scheduled or manual `run` retries it. A successful attempt appends history and removes the
queue entry in one transaction.

Timeout and cancellation terminate the Codex process group, record `attempt_timeout` or `attempt_canceled`, and leave
the queue entry. If Codex published before a local failure, the next attempt finds the exact marker and recovers the
existing review without publishing a duplicate.

## Check prerequisites

Run all local checks without publishing a review:

```shell
reviewctl --json doctor
```

`doctor` checks configuration, GitHub authentication and configured repository access, Codex login and attempt-option
compatibility without starting a model, the frozen APM skill, SQLite state, and required paths. It runs checks
sequentially and reports every prerequisite. A failed check returns exit code 1.

## Schedule with launchd

The [launchd plist example](docs/com.denifilatoff.reviewctl.plist) invokes `reviewctl --json run` every five minutes.
It is an example, not an installer.

1. Replace `/Users/you` with your absolute home path. Use these values for interactive and scheduled runs:

   ```shell
   export XDG_CONFIG_HOME=/Users/you/.config
   export XDG_STATE_HOME=/Users/you/.local/state
   export XDG_CACHE_HOME=/Users/you/.cache
   export XDG_RUNTIME_DIR=/Users/you/.local/state
   ```

   Keeping `XDG_RUNTIME_DIR` equal to `XDG_STATE_HOME` gives manual and scheduled runs the same lock path.

2. Install the binary at `/Users/you/.local/bin/reviewctl`, then initialize and check it:

   ```shell
   reviewctl --json init
   reviewctl --json doctor
   ```

3. Copy the plist to `~/Library/LaunchAgents/com.denifilatoff.reviewctl.plist`, replace every `/Users/you` value, and
   validate it:

   ```shell
   plutil -lint ~/Library/LaunchAgents/com.denifilatoff.reviewctl.plist
   ```

4. Bootstrap and trigger one cycle:

   ```shell
   launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.denifilatoff.reviewctl.plist
   launchctl kickstart -k "gui/$(id -u)/com.denifilatoff.reviewctl"
   ```

5. Check the job and local state:

   ```shell
   launchctl print "gui/$(id -u)/com.denifilatoff.reviewctl"
   reviewctl --json status
   ```

6. Remove the schedule without deleting local state:

   ```shell
   launchctl bootout "gui/$(id -u)" ~/Library/LaunchAgents/com.denifilatoff.reviewctl.plist
   ```

## Live E2E

`make live-e2e` can target only the permanent
[reviewctl pull request 24](https://github.com/denifilatoff/reviewctl/pull/24), authored by `denifilatoff`. Do not merge
or close this TEST fixture.

The script checks the exact repository, pull request, author, open non-draft state, pinned head, and head-specific
review marker count before invoking Codex. It accepts zero or one marker and rejects duplicates. Every run establishes
a discovery baseline and records a safe `publication_disabled` failure with the queue retained. It then enables
publication. A zero-marker run requires one new `COMMENT` review; a one-marker run requires recovery of that review.
Both modes re-enqueue the same head and require another recovery without a duplicate. Final assertions require one
failed and two successful history rows, plus an empty queue.

The authenticated reviewer also authored the fixture. GitHub forbids `APPROVE` and `REQUEST_CHANGES` for that
self-review, so `COMMENT` is the expected successful verdict. The first zero-marker run proves publication. Later runs
prove recovery without requiring a fixture head change. Missing credentials, an unsafe fixture, a changed head, or
duplicate exact markers fail the test.
