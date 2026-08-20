# Repository agent instructions

## Repository language

- Use English for all repository content, including source code, identifiers, comments, documentation, commit
  messages, branch names, pull request text, and review comments.

## Git workflow

- At the start of every agent session and immediately before every push, run `git fetch origin main`.
- After fetching, verify that `HEAD` equals `origin/main` when working on `main`. On any other branch, require
  `git merge-base --is-ancestor origin/main HEAD` to succeed. If either check fails, update the current branch from
  `origin/main` and repeat the check before continuing.
- Never commit or push directly to `main`. Create a dedicated branch for every change and merge it into `main` only
  through a pull request.
- Open every pull request as ready for review, not as a draft.
- Publish live E2E reviews only to the canonical fixture PR documented in `README.md` in `denifilatoff/reviewctl`,
  never another repository. Before Codex, recheck its identity, open non-draft state, and head; after publication,
  verify readback and duplicate-free marker recovery.
