#!/bin/sh
set -eu

label="top.luocaiyi.dst-admin-agent"
binary=""
config=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) binary=${2:?missing --binary value}; shift 2 ;;
    --config) config=${2:?missing --config value}; shift 2 ;;
    --label) label=${2:?missing --label value}; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Darwin" ]; then
  echo "this installer only supports macOS" >&2
  exit 1
fi
if [ -z "$binary" ] || [ -z "$config" ] || [ ! -f "$binary" ] || [ ! -f "$config" ]; then
  echo "usage: $0 --binary <dst-admin-agent> --config <agent.conf> [--label <launchd-label>]" >&2
  exit 2
fi

state_dir="$HOME/Library/Application Support/DST Admin Agent"
log_dir="$HOME/Library/Logs/DST Admin Agent"
launch_agents="$HOME/Library/LaunchAgents"
installed_binary="$state_dir/bin/dst-admin-agent"
installed_config="$state_dir/agent.conf"
state_file="$state_dir/runtime-state.json"
plist="$launch_agents/$label.plist"
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

mkdir -p "$state_dir/bin" "$log_dir" "$launch_agents"
chmod 700 "$state_dir" "$state_dir/bin" "$log_dir"
install -m 0755 "$binary" "$installed_binary"
install -m 0600 "$config" "$installed_config"

launchctl bootout "$domain/$label" >/dev/null 2>&1 || true
rotate_log "$log_dir/agent.log"
rotate_log "$log_dir/agent-error.log"
rm -f "$plist"
plutil -create xml1 "$plist"
/usr/libexec/PlistBuddy -c "Add :Label string $label" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments array" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:0 string $installed_binary" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:1 string -config" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:2 string $installed_config" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:3 string -state" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProgramArguments:4 string $state_file" "$plist"
/usr/libexec/PlistBuddy -c "Add :RunAtLoad bool true" "$plist"
/usr/libexec/PlistBuddy -c "Add :KeepAlive dict" "$plist"
/usr/libexec/PlistBuddy -c "Add :KeepAlive:SuccessfulExit bool false" "$plist"
/usr/libexec/PlistBuddy -c "Add :ProcessType string Background" "$plist"
/usr/libexec/PlistBuddy -c "Add :ThrottleInterval integer 10" "$plist"
/usr/libexec/PlistBuddy -c "Add :StandardOutPath string $log_dir/agent.log" "$plist"
/usr/libexec/PlistBuddy -c "Add :StandardErrorPath string $log_dir/agent-error.log" "$plist"
chmod 600 "$plist"
plutil -lint "$plist"
launchctl bootstrap "$domain" "$plist"
launchctl enable "$domain/$label"
launchctl kickstart -k "$domain/$label"

echo "installed $label"
echo "state: $state_dir"
echo "logs: $log_dir"
