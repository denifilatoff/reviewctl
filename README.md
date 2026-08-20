# reviewctl

`reviewctl` queues GitHub pull requests on one trusted laptop and asks Codex to publish reviews. The first working slice
accepts explicit pull request URLs. It does not discover changes automatically.

Build the command and run the CI gates from the repository root:

```shell
go build -o reviewctl ./cmd/reviewctl
make ci
```

## Configuration

Create `$XDG_CONFIG_HOME/reviewctl/config.yaml`. When `XDG_CONFIG_HOME` is unset, use
`~/.config/reviewctl/config.yaml`.

```yaml
harness: codex
publish: true
trusted_authors:
  - dependabot[bot]
repositories:
  - provider: github
    repository: owner/repository
```

`run` rejects repositories outside this list and authors outside `trusted_authors`. It also stops before Codex when
`publish` is false.

## Commands

Queue one pull request without invoking Codex:

```shell
reviewctl --json review https://github.com/owner/repository/pull/123
```

Process the queue snapshot once, sequentially, and exit:

```shell
reviewctl --json run
```

State is stored in `$XDG_STATE_HOME/reviewctl/reviewctl.db`, or `~/.local/state/reviewctl/reviewctl.db` when the XDG
variable is unset. `--json` writes exactly one result object to stdout. Exit code `0` means success or a safe no-op,
`1` means an operational failure, and `2` means invalid input.

## Live E2E

`make live-e2e` is restricted to `denifilatoff/gudwin` pull request 19 and `dependabot[bot]`. The script checks the
open, non-draft fixture and pins its head before it lets `run` publish. A second run must recover the marker without
creating a duplicate review. Missing credentials or an unsafe fixture fail the test.
