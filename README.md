# reviewctl

`reviewctl` is a local CLI that queues GitHub pull requests and uses Codex to publish reviews.

## Install

```shell
curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/scripts/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
reviewctl --help
```

`reviewctl` requires authenticated GitHub CLI (`gh`), Codex CLI, and APM.
For review cost estimates, install [ccusage](https://ccusage.com/guide/) 20.0.19 or later within the 20.x series.
No telemetry collector is required.

See the [operations guide](docs/operations.md) to configure repositories and run `reviewctl` on macOS.

Live E2E reviews are restricted to the canonical
[reviewctl pull request 24](https://github.com/denifilatoff/reviewctl/pull/24).
