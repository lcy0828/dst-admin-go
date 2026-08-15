#!/bin/sh
set -eu
umask 077

config=${DST_ADMIN_CONFIG:-/var/lib/dst-admin/app.conf}
mkdir -p \
  /var/lib/dst-admin/backups \
  /var/lib/dst-admin/maps \
  /var/lib/dst-admin/unmanaged-saves \
  /var/lib/dst-admin/unmanaged-server \
  /var/lib/dst-admin/unmanaged-ugc \
  /var/lib/dst-admin/workshop/steamapps/workshop/content/322330
if [ ! -e "$config" ]; then
  cp /usr/local/share/dst-admin/control-plane.conf "$config"
  chmod 0600 "$config"
fi
exec "$@"
