#!/bin/sh
set -eu

fail() {
	echo "$1" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || fail "launchd installation requires macOS"
[ "$#" -le 1 ] || fail "usage: install-launchd.sh [interval-seconds]"
interval=${1:-300}
case $interval in
'' | *[!0-9]*) fail "interval must be a positive integer" ;;
esac
[ "$interval" -gt 0 ] || fail "interval must be a positive integer"

home=${HOME:?HOME is required}
find_executable() {
	path=$(command -v "$1") || fail "$1 is required"
	case $path in
	/*) [ -x "$path" ] || fail "$1 is not executable: $path" ;;
	*) fail "$1 must resolve to an absolute path: $path" ;;
	esac
	printf '%s\n' "$path"
}

reviewctl=$(find_executable reviewctl)
gh=$(find_executable gh)
codex=$(find_executable codex)
apm=$(find_executable apm)
plutil=$(find_executable plutil)
launchctl=$(find_executable launchctl)

job_path=/usr/bin:/bin
for executable in "$reviewctl" "$gh" "$codex" "$apm"; do
	directory=${executable%/*}
	case :$job_path: in
	*:$directory:*) ;;
	*) job_path=$directory:$job_path ;;
	esac
done

config_home=${XDG_CONFIG_HOME:-$home/.config}
state_home=${XDG_STATE_HOME:-$home/.local/state}
cache_home=${XDG_CACHE_HOME:-$home/.cache}
runtime_dir=${XDG_RUNTIME_DIR:-$state_home}
label=com.denifilatoff.reviewctl
domain=gui/$(id -u)
service=$domain/$label
agents_dir=$home/Library/LaunchAgents
logs_dir=$home/Library/Logs
plist=$agents_dir/$label.plist
if [ -e "$plist" ]; then
	printf 'A launchd configuration already exists at %s. Replace it? [y/N] ' "$plist" >&2
	answer=
	IFS= read -r answer || true
	case $answer in
	y | Y | yes | Yes | YES) ;;
	*)
		echo "Existing launchd configuration was kept."
		exit 0
		;;
	esac
fi
mkdir -p "$agents_dir" "$logs_dir"

if ! env -i \
	HOME="$home" \
	PATH="$job_path" \
	XDG_CONFIG_HOME="$config_home" \
	XDG_STATE_HOME="$state_home" \
	XDG_CACHE_HOME="$cache_home" \
	XDG_RUNTIME_DIR="$runtime_dir" \
	"$reviewctl" --json doctor; then
	fail "reviewctl doctor failed in the launchd environment"
fi

xml_escape() {
	printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

escaped_reviewctl=$(xml_escape "$reviewctl")
escaped_path=$(xml_escape "$job_path")
escaped_config=$(xml_escape "$config_home")
escaped_state=$(xml_escape "$state_home")
escaped_cache=$(xml_escape "$cache_home")
escaped_runtime=$(xml_escape "$runtime_dir")
escaped_stdout=$(xml_escape "$logs_dir/reviewctl.out.log")
escaped_stderr=$(xml_escape "$logs_dir/reviewctl.err.log")
temporary_plist=$(mktemp "$plist.tmp.XXXXXX")
cleanup() {
	[ -z "$temporary_plist" ] || rm -f "$temporary_plist"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

cat >"$temporary_plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$escaped_reviewctl</string>
    <string>--json</string>
    <string>run</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>$escaped_path</string>
    <key>XDG_CONFIG_HOME</key>
    <string>$escaped_config</string>
    <key>XDG_STATE_HOME</key>
    <string>$escaped_state</string>
    <key>XDG_CACHE_HOME</key>
    <string>$escaped_cache</string>
    <key>XDG_RUNTIME_DIR</key>
    <string>$escaped_runtime</string>
  </dict>
  <key>StartInterval</key>
  <integer>$interval</integer>
  <key>StandardOutPath</key>
  <string>$escaped_stdout</string>
  <key>StandardErrorPath</key>
  <string>$escaped_stderr</string>
</dict>
</plist>
EOF
chmod 600 "$temporary_plist"
"$plutil" -lint "$temporary_plist" >/dev/null

if "$launchctl" print "$service" >/dev/null 2>&1; then
	"$launchctl" bootout "$service"
fi
mv "$temporary_plist" "$plist"
temporary_plist=
"$launchctl" bootstrap "$domain" "$plist"
"$launchctl" kickstart -k "$service"
printf 'Installed %s with a %s-second interval.\n' "$plist" "$interval"
