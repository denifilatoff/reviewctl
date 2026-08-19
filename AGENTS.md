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
- Use Conventional Commits format for commit messages and pull request titles: `type: summary` or
  `type(scope): summary`. Use `feat` for new behavior, `fix` for bugs, and the precise supporting type when
  applicable: `docs`, `test`, `refactor`, `perf`, `build`, `ci`, `chore`, or `revert`.

## Architecture decision follow-up

- After completing an implementation issue, follow `docs/ADR/README.md` to inspect available ignored Superpowers
  documents and the available user-agent conversation for ADR candidates. Do not commit raw artifacts or transcripts,
  and do not reconstruct unavailable conversation content.
- Before closing the issue, post the candidates or `ADR candidates: none` to its GitHub thread. Do not create an ADR
  until the repository owner approves the candidate.
