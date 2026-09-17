#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$script_dir/native-common.sh"

usage() {
  echo "usage: $0 --binary PATH --config PATH [--web-root PATH] [--renderer PATH] [--user dst]" >&2
  exit 64
}

binary=""
config=""
web_root=""
renderer=""
service_user="dst"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --binary) [ "$#" -ge 2 ] || usage; binary=$2; shift 2 ;;
    --config) [ "$#" -ge 2 ] || usage; config=$2; shift 2 ;;
    --web-root) [ "$#" -ge 2 ] || usage; web_root=$2; shift 2 ;;
    --renderer) [ "$#" -ge 2 ] || usage; renderer=$2; shift 2 ;;
    --user) [ "$#" -ge 2 ] || usage; service_user=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "run as root" >&2; exit 77; }
[ -f "$binary" ] && [ -x "$binary" ] || { echo "invalid DST Admin binary" >&2; exit 66; }
[ -f "$config" ] || { echo "invalid local configuration" >&2; exit 66; }
if [ -n "$web_root" ]; then
  [ -f "$web_root/index.html" ] || { echo "web root does not contain index.html" >&2; exit 66; }
else
  "$binary" -version | grep -Eq '"embeddedWebUI"[[:space:]]*:[[:space:]]*true' || { echo "binary has no embedded UI; pass --web-root" >&2; exit 66; }
fi
ensure_configured_steamcmd "$config"
case "$service_user" in ""|*[!A-Za-z0-9_-]*) echo "invalid service user" >&2; exit 64 ;; esac
id "$service_user" >/dev/null 2>&1 || { echo "service user does not exist: $service_user" >&2; exit 67; }
service_group=$(id -gn "$service_user")
service_shell=$(getent passwd "$service_user" | awk -F: '{print $7}')
case "$service_shell" in
  ""|*/false|*/nologin) echo "service user must have an executable login shell for tmux: $service_user" >&2; exit 68 ;;
esac
[ -x "$service_shell" ] || { echo "service user shell is not executable: $service_shell" >&2; exit 68; }
command -v tmux >/dev/null 2>&1 || { echo "tmux is required" >&2; exit 69; }

install -d -o "$service_user" -g "$service_group" -m 0700 /var/lib/dst-admin
install -d -o "$service_user" -g "$service_group" -m 0700 /var/lib/dst-admin/home
if [ -n "$web_root" ]; then
  install -d -o root -g root -m 0755 /usr/share/dst-admin/web
  cp -a "$web_root/." /usr/share/dst-admin/web/
  chown -R root:root /usr/share/dst-admin/web
  find /usr/share/dst-admin/web -type d -exec chmod 0755 {} +
  find /usr/share/dst-admin/web -type f -exec chmod 0644 {} +
fi

install -o root -g root -m 0755 "$binary" /usr/local/bin/dst-admin
install -o "$service_user" -g "$service_group" -m 0600 "$config" /var/lib/dst-admin/app.conf
if [ -n "$renderer" ]; then
  [ -f "$renderer" ] && [ -x "$renderer" ] || { echo "invalid map renderer binary" >&2; exit 66; }
  install -o root -g root -m 0755 "$renderer" /usr/local/bin/dst-map-renderer
fi
unit_template="$(dirname "$0")/../systemd/dst-admin-local.service"
sed \
  -e "s/^User=dst$/User=$service_user/" \
  -e "s/^Group=dst$/Group=$service_group/" \
  "$unit_template" >/etc/systemd/system/dst-admin-local.service
if [ -n "$web_root" ]; then
  sed -i '/^\[Service\]/a Environment=DST_ADMIN_WEB_ROOT=/usr/share/dst-admin/web' /etc/systemd/system/dst-admin-local.service
fi
chmod 0644 /etc/systemd/system/dst-admin-local.service
systemctl daemon-reload
echo "installed local mode; run: systemctl enable --now dst-admin-local"
