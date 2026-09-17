#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$script_dir/native-common.sh"

usage() {
  echo "usage: $0 --binary PATH --config PATH [--user dst]" >&2
  exit 64
}

binary=""
config=""
service_user=dst
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) [ "$#" -ge 2 ] || usage; binary=$2; shift 2 ;;
    --config) [ "$#" -ge 2 ] || usage; config=$2; shift 2 ;;
    --user) [ "$#" -ge 2 ] || usage; service_user=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "run as root" >&2; exit 77; }
[ -f "$binary" ] && [ -x "$binary" ] || { echo "invalid Agent binary" >&2; exit 66; }
[ -f "$config" ] || { echo "invalid Agent config" >&2; exit 66; }
ensure_configured_steamcmd "$config"
case "$service_user" in ""|*[!A-Za-z0-9_-]*) echo "invalid service user" >&2; exit 64 ;; esac
id "$service_user" >/dev/null 2>&1 || { echo "service user does not exist: $service_user" >&2; exit 67; }
service_group=$(id -gn "$service_user")
service_shell=$(getent passwd "$service_user" | awk -F: '{print $7}')
case "$service_shell" in
  ""|*/false|*/nologin) echo "service user must have an executable login shell for tmux: $service_user" >&2; exit 68 ;;
esac
[ -x "$service_shell" ] || { echo "service user shell is not executable: $service_shell" >&2; exit 68; }

install -d -o "$service_user" -g "$service_group" -m 0700 /var/lib/dst-admin-agent
install -d -o "$service_user" -g "$service_group" -m 0700 /var/lib/dst-admin-agent/bin
install -d -o "$service_user" -g "$service_group" -m 0700 /var/lib/dst-admin-agent/home
install -d -o root -g "$service_group" -m 0750 /etc/dst-admin
install -o "$service_user" -g "$service_group" -m 0755 "$binary" /var/lib/dst-admin-agent/bin/dst-admin-agent
install -o "$service_user" -g "$service_group" -m 0600 "$config" /var/lib/dst-admin-agent/agent.conf
unit_template="$(dirname "$0")/../systemd/dst-admin-agent.service"
sed \
  -e "s/^User=dst$/User=$service_user/" \
  -e "s/^Group=dst$/Group=$service_group/" \
  "$unit_template" >/etc/systemd/system/dst-admin-agent.service
chmod 0644 /etc/systemd/system/dst-admin-agent.service
systemctl daemon-reload
echo "installed with page-controlled upgrades enabled"
echo "review /var/lib/dst-admin-agent/agent.conf, then run: systemctl enable --now dst-admin-agent"
