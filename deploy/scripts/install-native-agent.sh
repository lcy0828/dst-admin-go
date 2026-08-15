#!/bin/sh
set -eu

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
case "$service_user" in ""|*[!A-Za-z0-9_-]*) echo "invalid service user" >&2; exit 64 ;; esac
id "$service_user" >/dev/null 2>&1 || { echo "service user does not exist: $service_user" >&2; exit 67; }

install -d -o "$service_user" -g "$service_user" -m 0700 /var/lib/dst-admin-agent
install -d -o root -g "$service_user" -m 0750 /etc/dst-admin
install -o root -g root -m 0755 "$binary" /usr/local/bin/dst-admin-agent
install -o root -g "$service_user" -m 0600 "$config" /etc/dst-admin/agent.conf
install -o root -g root -m 0644 "$(dirname "$0")/../systemd/dst-admin-agent.service" /etc/systemd/system/dst-admin-agent.service
systemctl daemon-reload
echo "installed; review /etc/dst-admin/agent.conf, then run: systemctl enable --now dst-admin-agent"
