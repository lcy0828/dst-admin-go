#!/bin/sh
set -eu

usage() {
  echo "usage: $0 --binary PATH --config PATH [--web-root PATH] [--renderer PATH]" >&2
  exit 64
}

[ "$(uname -s)" = "Darwin" ] || { echo "this installer only supports macOS" >&2; exit 69; }
[ "$(id -u)" != "0" ] || { echo "run this installer as the signed-in user, not root" >&2; exit 77; }

binary=""
config=""
web_root=""
renderer=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) [ "$#" -ge 2 ] || usage; binary=$2; shift 2 ;;
    --config) [ "$#" -ge 2 ] || usage; config=$2; shift 2 ;;
    --web-root) [ "$#" -ge 2 ] || usage; web_root=$2; shift 2 ;;
    --renderer) [ "$#" -ge 2 ] || usage; renderer=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ -f "$binary" ] && [ -x "$binary" ] || { echo "invalid DST Admin binary" >&2; exit 66; }
[ -f "$config" ] || { echo "invalid local configuration" >&2; exit 66; }
if [ -n "$web_root" ]; then
  [ -f "$web_root/index.html" ] || { echo "web root does not contain index.html" >&2; exit 66; }
else
  "$binary" -version | grep -Eq '"embeddedWebUI"[[:space:]]*:[[:space:]]*true' || { echo "binary has no embedded UI; pass --web-root" >&2; exit 66; }
fi
command -v tmux >/dev/null 2>&1 || { echo "tmux is required" >&2; exit 69; }

label=top.luocaiyi.dst-admin-local
state_dir="$HOME/Library/Application Support/DST Admin"
bin_dir="$state_dir/bin"
installed_web="$state_dir/web"
log_dir="$state_dir/logs"
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"

rotate_log() {
  log_path=$1
  [ -f "$log_path" ] || return 0
  log_size=$(stat -f %z "$log_path")
  [ "$log_size" -lt 20971520 ] || {
    tail -c 20971520 "$log_path" >"$log_path.previous"
    chmod 0600 "$log_path.previous"
    : >"$log_path"
  }
}

install -d -m 0700 "$state_dir" "$bin_dir" "$installed_web" "$log_dir"
install -m 0755 "$binary" "$bin_dir/dst-admin"
install -m 0600 "$config" "$state_dir/app.conf"
if [ -n "$web_root" ]; then cp -a "$web_root/." "$installed_web/"; fi
if [ -n "$renderer" ]; then
  [ -f "$renderer" ] && [ -x "$renderer" ] || { echo "invalid map renderer binary" >&2; exit 66; }
  install -m 0755 "$renderer" "$bin_dir/dst-map-renderer"
fi
install -d -m 0755 "$HOME/Library/LaunchAgents"
launchctl bootout "$domain/$label" >/dev/null 2>&1 || true
rotate_log "$log_dir/stdout.log"
rotate_log "$log_dir/stderr.log"
rm -f "$plist"
plutil -create xml1 "$plist"
/usr/libexec/PlistBuddy -c "Add :Label string $label" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments array" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:0 string $bin_dir/dst-admin" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:1 string -addr" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:2 string 127.0.0.1:8000" "$plist"
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables dict" "$plist"
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:DST_ADMIN_CONFIG string $state_dir/app.conf" "$plist"
if [ -n "$web_root" ]; then
  /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:DST_ADMIN_WEB_ROOT string $installed_web" "$plist"
fi
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:GIN_MODE string release" "$plist"
/usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:PATH string /opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" "$plist"
/usr/libexec/PlistBuddy -c "Add :WorkingDirectory string $state_dir" "$plist"
/usr/libexec/PlistBuddy -c "Add :RunAtLoad bool true" "$plist"
/usr/libexec/PlistBuddy -c "Add :KeepAlive dict" "$plist"
/usr/libexec/PlistBuddy -c "Add :KeepAlive:SuccessfulExit bool false" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProcessType string Background" "$plist"
/usr/libexec/PlistBuddy -c "Add :ThrottleInterval integer 10" "$plist"
/usr/libexec/PlistBuddy -c "Add :StandardOutPath string $log_dir/stdout.log" "$plist"
/usr/libexec/PlistBuddy -c "Add :StandardErrorPath string $log_dir/stderr.log" "$plist"
chmod 0600 "$plist"
plutil -lint "$plist"
launchctl bootstrap "$domain" "$plist"
launchctl enable "$domain/$label"
launchctl kickstart -k "$domain/$label"
echo "installed local mode at http://127.0.0.1:8000"
