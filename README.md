# reviewctl

`reviewctl` is a local CLI that queues GitHub pull requests and uses Codex to publish reviews.

## Install

```shell
curl -fsSL https://raw.githubusercontent.com/denifilatoff/reviewctl/main/scripts/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
reviewctl --help
```

`reviewctl` requires authenticated GitHub CLI (`gh`), Codex CLI, and APM.

See the [operations guide](docs/operations.md) to configure repositories and run `reviewctl` on macOS.

Live E2E reviews are restricted to the canonical
[reviewctl pull request 24](https://github.com/denifilatoff/reviewctl/pull/24).
