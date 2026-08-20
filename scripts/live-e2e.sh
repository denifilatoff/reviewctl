#!/bin/sh
set -eu

pr_url=${REVIEWCTL_LIVE_PR_URL:-https://github.com/denifilatoff/reviewctl/pull/24}
repository=${REVIEWCTL_LIVE_REPOSITORY:-denifilatoff/reviewctl}
trusted_author=${REVIEWCTL_LIVE_TRUSTED_AUTHOR:-denifilatoff}
binary=${REVIEWCTL_BIN:-}

if [ "$pr_url" != "https://github.com/denifilatoff/reviewctl/pull/24" ] || \
    [ "$repository" != "denifilatoff/reviewctl" ] || [ "$trusted_author" != "denifilatoff" ]; then
  echo "live E2E is restricted to denifilatoff/reviewctl PR #24 authored by denifilatoff" >&2
  exit 2
fi
if [ -z "$binary" ] || [ ! -x "$binary" ]; then
  echo "REVIEWCTL_BIN must name the built reviewctl executable" >&2
  exit 2
fi
for command in gh apm codex sqlite3; do
  command -v "$command" >/dev/null || { echo "$command is required" >&2; exit 1; }
done
gh auth status >/dev/null
codex login status >/dev/null

preflight=$(gh pr view "$pr_url" --json state,isDraft,author,headRefOid \
  --jq '[.state, (.isDraft|tostring), .author.login, .headRefOid] | @tsv')
IFS=$(printf '\t') read -r state draft author head <<EOF
$preflight
EOF
rest_author=$(gh api repos/denifilatoff/reviewctl/pulls/24 --jq .user.login)
rest_head=$(gh api repos/denifilatoff/reviewctl/pulls/24 --jq .head.sha)
if [ "$state" != "OPEN" ] || [ "$draft" != "false" ] || [ "$author" != "$trusted_author" ]; then
  echo "unsafe live fixture: state=$state draft=$draft author=$author" >&2
  exit 1
fi
if [ "$rest_author" != "$trusted_author" ] || [ "$rest_head" != "$head" ]; then
  echo "REST preflight disagrees with gh pr view: author=$rest_author head=$rest_head" >&2
  exit 1
fi
printf '%s\n' "$head" | grep -Eq '^[0-9a-f]{40}$' || { echo "invalid pinned head: $head" >&2; exit 1; }

work=$(mktemp -d)
cleanup() { rm -rf "$work"; }
trap cleanup EXIT HUP INT TERM
mkdir -p "$work/config/reviewctl" "$work/state"
write_config() {
  cat >"$work/config/reviewctl/config.yaml" <<EOF
harness: codex
publish: $1
trusted_authors:
  - denifilatoff
repositories:
  - provider: github
    repository: denifilatoff/reviewctl
EOF
}
write_config false
export XDG_CONFIG_HOME="$work/config"
export XDG_STATE_HOME="$work/state"

marker_prefix="<!-- reviewctl:github:denifilatoff/reviewctl#24:$head:"
count_markers() {
  gh api repos/denifilatoff/reviewctl/pulls/24/reviews --paginate \
    --jq ".[] | select(.body != null and (.body | contains(\"$marker_prefix\"))) | .id" |
    awk 'NF { count++ } END { print count + 0 }'
}

before_count=$(count_markers)
case "$before_count" in
  0) mode=publication ;;
  1) mode=recovery ;;
  *)
    echo "unsafe live fixture: expected at most one exact review marker for head $head, found $before_count" >&2
    exit 1
    ;;
esac

"$binary" --json run >"$work/baseline.json"
if ! grep -q '"discovery_succeeded":1' "$work/baseline.json" ||
    ! grep -q '"queued":0' "$work/baseline.json" || ! grep -q '"attempted":0' "$work/baseline.json"; then
  echo "first run did not establish an empty discovery baseline" >&2
  exit 1
fi
baseline_count=$(sqlite3 "$work/state/reviewctl/reviewctl.db" \
  "SELECT COUNT(*) FROM repository_baselines WHERE provider = 'github' AND repository = 'denifilatoff/reviewctl';")
if [ "$baseline_count" -ne 1 ]; then
  echo "discovery baseline was not recorded" >&2
  exit 1
fi

"$binary" --json review "$pr_url" >"$work/enqueue-retry.json"
if "$binary" --json run >"$work/run-retry.json"; then
  echo "publication-disabled retry unexpectedly succeeded" >&2
  exit 1
fi
if ! grep -q '"code":"publication_disabled"' "$work/run-retry.json"; then
  echo "safe retry did not report publication_disabled" >&2
  exit 1
fi
retry_history=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM history;')
retry_queue=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM queue;')
if [ "$retry_history" -ne 1 ] || [ "$retry_queue" -ne 1 ]; then
  echo "safe retry did not retain queue/history: history=$retry_history queue=$retry_queue" >&2
  exit 1
fi

write_config true
"$binary" --json run >"$work/run-1.json"
first_count=$(count_markers)
if [ "$first_count" -ne 1 ] || ! grep -q '"verdict":"COMMENT"' "$work/run-1.json"; then
  echo "first successful run did not read back exactly one COMMENT review" >&2
  exit 1
fi
if [ "$mode" = publication ] && ! grep -q '"recovered":false' "$work/run-1.json"; then
  echo "zero-marker run did not publish a new review" >&2
  exit 1
fi
if [ "$mode" = recovery ] && ! grep -q '"recovered":true' "$work/run-1.json"; then
  echo "existing-marker run did not recover the review" >&2
  exit 1
fi
if [ "$(gh pr view "$pr_url" --json headRefOid --jq .headRefOid)" != "$head" ]; then
  echo "pull request head changed after the first run" >&2
  exit 1
fi

"$binary" --json review "$pr_url" >"$work/enqueue-2.json"
"$binary" --json run >"$work/run-2.json"
second_count=$(count_markers)
if [ "$second_count" -ne 1 ] || ! grep -q '"recovered":true' "$work/run-2.json" ||
    ! grep -q '"verdict":"COMMENT"' "$work/run-2.json"; then
  echo "second run did not recover the existing marker without a duplicate" >&2
  exit 1
fi
if [ "$(gh pr view "$pr_url" --json headRefOid --jq .headRefOid)" != "$head" ]; then
  echo "pull request head changed after marker recovery" >&2
  exit 1
fi

history_count=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM history;')
success_count=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM history WHERE success = 1;')
failure_count=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM history WHERE success = 0;')
queue_count=$(sqlite3 "$work/state/reviewctl/reviewctl.db" 'SELECT COUNT(*) FROM queue;')
if [ "$history_count" -ne 3 ] || [ "$success_count" -ne 2 ] || [ "$failure_count" -ne 1 ] ||
    [ "$queue_count" -ne 0 ]; then
  echo "unexpected state after live E2E: history=$history_count success=$success_count failure=$failure_count queue=$queue_count" >&2
  exit 1
fi
printf 'live E2E passed: mode=%s head=%s marked_reviews=%s history=%s\n' \
  "$mode" "$head" "$second_count" "$history_count"
