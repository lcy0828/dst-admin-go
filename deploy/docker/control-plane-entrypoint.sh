#!/bin/sh
set -eu
umask 077

config=${DST_ADMIN_CONFIG:-/var/lib/dst-admin/app.conf}
if [ ! -e "$config" ]; then
  cp /usr/local/share/dst-admin/control-plane.conf "$config"
  chmod 0600 "$config"
fi
exec "$@"
