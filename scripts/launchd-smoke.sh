#!/bin/sh
set -eu

if [ "$(uname -s)" != Darwin ]; then
	echo "launchd smoke requires macOS" >&2
	exit 1
fi
binary=${REVIEWCTL_BIN:-}
if [ -z "$binary" ] || [ ! -x "$binary" ]; then
	echo "REVIEWCTL_BIN must name the built reviewctl executable" >&2
	exit 2
fi
command -v launchctl >/dev/null || { echo "launchctl is required" >&2; exit 1; }

uid=$(id -u)
domain="gui/$uid"
temporary_root=${TMPDIR:-/tmp}
work=$(mktemp -d "$temporary_root/reviewctl-launchd-smoke.XXXXXX")
label="com.denifilatoff.reviewctl.smoke.$uid.$$"
service="$domain/$label"
cleanup() {
	status=$1
	trap - EXIT
	if ! launchctl bootout "$service" >/dev/null 2>&1; then
		echo "launchd smoke could not unload temporary job" >&2
		status=1
	fi
	if ! rm -rf "$work"; then
		echo "launchd smoke could not remove temporary workspace: $work" >&2
		status=1
	fi
	return "$status"
}
trap 'cleanup $?' EXIT
trap 'exit 1' HUP INT TERM

mkdir -p "$work/bin" "$work/config/reviewctl" "$work/state" "$work/cache" "$work/runtime" "$work/tmp"
cat >"$work/config/reviewctl/config.yaml" <<'EOF'
harness: codex
publish: false
trusted_authors: [reviewctl-smoke]
repositories:
  - provider: github
    repository: example/smoke
EOF
cat >"$work/bin/gh" <<'EOF'
#!/bin/sh
set -eu
if [ "$#" -eq 10 ] && [ "$1" = pr ] && [ "$2" = list ] && [ "$3" = --repo ] && \
    [ "$4" = example/smoke ] && [ "$5" = --state ] && [ "$6" = open ] && \
    [ "$7" = --limit ] && [ "$8" = 1001 ] && [ "$9" = --json ] && \
    [ "${10}" = url,number,state,isDraft,headRefOid,author ]; then
	printf '[]\n'
	exit 0
fi
echo "unexpected gh invocation" >&2
exit 64
EOF
chmod 700 "$work/bin/gh"

plist="$work/$label.plist"
stdout="$work/reviewctl.out.log"
stderr="$work/reviewctl.err.log"
cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$binary</string>
    <string>--json</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>$work/bin:/usr/bin:/bin</string>
    <key>TMPDIR</key>
    <string>$work/tmp</string>
    <key>XDG_CONFIG_HOME</key>
    <string>$work/config</string>
    <key>XDG_STATE_HOME</key>
    <string>$work/state</string>
    <key>XDG_CACHE_HOME</key>
    <string>$work/cache</string>
    <key>XDG_RUNTIME_DIR</key>
    <string>$work/runtime</string>
  </dict>
  <key>StandardOutPath</key>
  <string>$stdout</string>
  <key>StandardErrorPath</key>
  <string>$stderr</string>
</dict>
</plist>
EOF
chmod 600 "$plist"

launchctl bootstrap "$domain" "$plist"
launchctl kickstart -k "$service"
count=0
while [ ! -s "$stdout" ] && [ "$count" -lt 200 ]; do
	sleep 0.1
	count=$((count + 1))
done
if [ ! -s "$stdout" ]; then
	echo "launchd smoke timed out waiting for reviewctl" >&2
	[ ! -s "$stderr" ] || cat "$stderr" >&2
	exit 1
fi
for field in '"command":"run"' '"status":"success"' '"discovery_succeeded":1' \
    '"discovery_failed":0' '"queued":0' '"attempted":0'; do
	if ! grep -Fq "$field" "$stdout"; then
		echo "launchd smoke returned an unexpected result" >&2
		cat "$stdout" >&2
		[ ! -s "$stderr" ] || cat "$stderr" >&2
		exit 1
	fi
done
cleanup 0
printf 'launchd smoke passed: discovery_succeeded=1 queued=0 attempted=0\n'
