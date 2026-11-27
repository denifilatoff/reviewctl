# reviewctl

`reviewctl` is a local CLI that queues GitHub pull requests, uses Codex to analyze them, and publishes the reviews.

## Using ReviewCTL on a pull request

**To decline a finding, click Resolve conversation on its GitHub thread.** A comment is optional.
ReviewCTL treats a manual resolution as your decision on that finding. It leaves the thread closed and skips the
finding in later reviews, including their verdicts and summaries. Other findings on the PR remain active.

If you want the reviewer to reconsider a finding, leave the thread open and reply with your reasoning.
A new text reply from the PR author in an open ReviewCTL thread triggers a review, even on the same commit.
Replies received before the round starts are reviewed together. Reactions are ignored.

### Commands

Post the command as the entire text of a PR comment or a reply in a review thread. ReviewCTL reads commands on
its next poll and accepts them from accounts in the configured `trusted_authors` list.

| Command | What happens |
| --- | --- |
| `/reviewctl pause` | Pauses this PR. A running pass finishes and is saved. Publication and further passes wait. |
| `/reviewctl resume` | Resumes automation and handles pending pushes, replies or a saved review. With nothing pending, it waits for the next change. |
| `/reviewctl review` | Requests one full review of the current PR, even on the same commit. During a pause, it permits one review and leaves the PR paused afterward. |

### Finding IDs and automatic resolution

ReviewCTL assigns each finding an ID such as `F3`. The number belongs to that finding for the life of the PR,
across commits and review rounds. Existing open ReviewCTL threads are linked to their IDs on migration.

Each new round checks the current PR diff and the open findings. When ReviewCTL verifies a fix, it replies in its
thread, records the commit where it verified the fix, and resolves the conversation. If later changes bring the
same defect back, ReviewCTL can reopen a thread it previously closed and keep the same finding ID.

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
