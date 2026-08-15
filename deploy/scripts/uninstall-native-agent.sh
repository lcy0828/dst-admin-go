#!/bin/sh
set -eu

purge_state=false
if [ "${1:-}" = "--purge-state" ]; then
  purge_state=true
elif [ "$#" -gt 0 ]; then
  echo "usage: $0 [--purge-state]" >&2
  exit 2
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi

systemctl disable --now dst-admin-agent.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/dst-admin-agent.service
systemctl daemon-reload
rm -f /usr/local/bin/dst-admin-agent
if [ "$purge_state" = true ]; then
  rm -rf /var/lib/dst-admin-agent /etc/dst-admin/agent.conf /etc/dst-admin/agent.env
fi
echo "uninstalled dst-admin-agent; configuration and state were preserved unless --purge-state was supplied"
