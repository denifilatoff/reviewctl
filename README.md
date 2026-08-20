# reviewctl

`reviewctl` queues GitHub pull requests on one trusted laptop and asks Codex to publish reviews. It discovers review
events in configured repositories and also accepts explicit pull request URLs.

Build the command and run the CI gates from the repository root:

```shell
go build -o reviewctl .
make ci
```

## Configuration

Create `$XDG_CONFIG_HOME/reviewctl/config.yaml`. When `XDG_CONFIG_HOME` is unset, use
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

`run` rejects repositories outside this list and authors outside `trusted_authors`. It also stops before Codex when
`publish` is false. `attempt_timeout` defaults to one hour and accepts a positive Go duration up to 24 hours.

## Commands

Queue one pull request without invoking Codex:

```shell
reviewctl --json review https://github.com/owner/repository/pull/123
```

Discover changes, process the resulting queue snapshot once, sequentially, and exit:

```shell
reviewctl --json run
```

Check all local prerequisites without publishing a review:

```shell
reviewctl --json doctor
```

`doctor` checks the configuration, GitHub credentials and repository access, Codex login, the frozen APM skills,
SQLite state, and required paths. It runs the checks sequentially and reports every failed prerequisite.

State is stored in `$XDG_STATE_HOME/reviewctl/reviewctl.db`, or `~/.local/state/reviewctl/reviewctl.db` when the XDG
variable is unset. `--json` writes exactly one result object to stdout. Exit code `0` means success or a safe no-op,
`1` means an operational failure, and `2` means invalid input.

`status` includes `run_active`. A competing `run` returns `already_running` with exit code `0`; queue producers and
`status` remain available while the active run owns the machine-wide lock.

## launchd scheduling

The [launchd plist example](docs/com.denifilatoff.reviewctl.plist) invokes the stateless
`reviewctl --json run` cycle every five minutes. The process lock permits one `run` per machine. The plist is an
example, not an installer.

1. Build `reviewctl`, copy it to `/Users/<user>/.local/bin/reviewctl`, and run `reviewctl --json doctor`.
2. Copy the plist to `~/Library/LaunchAgents/com.denifilatoff.reviewctl.plist`.
3. Replace every `/Users/you` path in the copied plist with your absolute home path, then validate it:

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

`make live-e2e` is restricted to the permanent
[reviewctl pull request 24](https://github.com/denifilatoff/reviewctl/pull/24), authored by `denifilatoff`. Do not merge
or close this TEST fixture. The script checks its identity, open non-draft state, and head before it lets `run` publish.
It then verifies publication, readback, and recovery without a duplicate.

The authenticated reviewer also authored the fixture. GitHub therefore forbids `APPROVE` and `REQUEST_CHANGES` for
this self-review, so `COMMENT` is the expected successful verdict. Missing credentials or an unsafe fixture fail the
test.
