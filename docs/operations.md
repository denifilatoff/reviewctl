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
publish: false
attempt_timeout: 1h

trusted_authors:
  - <github-login>

repositories:
  - provider: github
    repository: <owner>/<repository>
```

Only `github` and `codex` are valid values. `attempt_timeout` is optional, defaults to `1h`, and must be greater than
zero and no more than `24h`.

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
failed discovery or review attempt makes `run` exit with code `1`; failed review entries remain queued for a later run.

## Use JSON and exit codes

Place `--json` before the command when a script needs one result object on standard output.

```shell
reviewctl --json doctor
reviewctl --json status --limit 50
```

Exit code `0` means success or a safe no-op, such as `already_queued` or `already_running`. Exit code `1` means an
operational failure. Exit code `2` means invalid syntax or input. In JSON mode, both successful and failed results go
to standard output; human-readable failures go to standard error.

## Schedule with launchd

On macOS, use the tracked
[launchd plist](https://raw.githubusercontent.com/denifilatoff/reviewctl/main/docs/com.denifilatoff.reviewctl.plist).
It runs `reviewctl --json run` every 300 seconds and writes logs under `~/Library/Logs`. Run `reviewctl doctor` first.

Download the plist, replace its `/Users/you` paths, validate it, and load it into your GUI domain.

```shell
label=com.denifilatoff.reviewctl
plist="$HOME/Library/LaunchAgents/$label.plist"
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/docs/com.denifilatoff.reviewctl.plist \
  -o "$plist"
sed -i '' "s#/Users/you#$HOME#g" "$plist"
plutil -lint "$plist"
launchctl bootstrap "gui/$(id -u)" "$plist"
launchctl kickstart -k "gui/$(id -u)/$label"
```

Inspect the loaded job and its logs with:

```shell
launchctl print "gui/$(id -u)/com.denifilatoff.reviewctl"
tail -n 100 "$HOME/Library/Logs/reviewctl.out.log"
tail -n 100 "$HOME/Library/Logs/reviewctl.err.log"
```

Remove the scheduled job without deleting the binary, configuration, or SQLite state:

```shell
label=com.denifilatoff.reviewctl
plist="$HOME/Library/LaunchAgents/$label.plist"
launchctl bootout "gui/$(id -u)/$label"
rm -f "$plist"
```
