#!/bin/sh
set -eu

label="top.luocaiyi.dst-admin-agent"
purge_state=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --label) label=${2:?missing --label value}; shift 2 ;;
    --purge-state) purge_state=true; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Darwin" ]; then
  echo "this uninstaller only supports macOS" >&2
  exit 1
fi

domain="gui/$(id -u)"
plist="$HOME/Library/LaunchAgents/$label.plist"
launchctl bootout "$domain/$label" >/dev/null 2>&1 || launchctl bootout "$domain" "$plist" >/dev/null 2>&1 || true
rm -f "$plist"
if [ "$purge_state" = true ]; then
  state_dir="$HOME/Library/Application Support/DST Admin Agent"
  case "$state_dir" in
    "$HOME/Library/Application Support/DST Admin Agent") rm -rf "$state_dir" ;;
    *) echo "refusing unsafe state path: $state_dir" >&2; exit 1 ;;
  esac
fi
echo "uninstalled $label"
