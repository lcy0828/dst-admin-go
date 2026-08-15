#!/bin/sh
set -eu
umask 077

config=/var/lib/dst-admin-agent/agent.conf
if [ ! -e "$config" ]; then
  cp /usr/local/share/dst-admin/agent.conf "$config"
  chmod 0600 "$config"
fi
exec "$@"
