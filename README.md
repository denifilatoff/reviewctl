# reviewctl

`reviewctl` queues GitHub pull requests on one trusted laptop and asks Codex to publish reviews. The first working slice
accepts explicit pull request URLs. It does not discover changes automatically.

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

`make live-e2e` is restricted to the permanent
[reviewctl pull request 24](https://github.com/denifilatoff/reviewctl/pull/24), authored by `denifilatoff`. Do not merge
or close this TEST fixture. The script checks its identity, open non-draft state, and head before it lets `run` publish.
It then verifies publication, readback, and recovery without a duplicate.

The authenticated reviewer also authored the fixture. GitHub therefore forbids `APPROVE` and `REQUEST_CHANGES` for
this self-review, so `COMMENT` is the expected successful verdict. Missing credentials or an unsafe fixture fail the
test.
