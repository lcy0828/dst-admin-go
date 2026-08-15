#!/bin/sh
set -eu

cluster=${DST_CLUSTER:-}
shard=${DST_SHARD:-}
storage_root=${DST_STORAGE_ROOT:-/data}
conf_dir=${DST_CONF_DIR:-DoNotStarveTogether}
server_root=${DST_SERVER_ROOT:-/opt/dst/server}
executable=${DST_EXECUTABLE:-$server_root/bin64/dontstarve_dedicated_server_nullrenderer_x64}

case "$cluster:$shard:$conf_dir" in
  *[!A-Za-z0-9_.:-]*) echo "cluster, shard or conf dir contains unsafe characters" >&2; exit 64 ;;
esac
[ -n "$cluster" ] && [ -n "$shard" ] && [ -n "$conf_dir" ] || { echo "DST_CLUSTER, DST_SHARD and DST_CONF_DIR are required" >&2; exit 64; }
case "$storage_root:$server_root:$executable" in
  *"
"*|*""*) echo "runtime path contains a newline" >&2; exit 64 ;;
esac
case "$storage_root:$server_root:$executable" in
  /*:/*:/*) ;;
  *) echo "runtime paths must be absolute" >&2; exit 64 ;;
esac

if [ "${DST_ADMIN_VALIDATE_ONLY:-0}" = "1" ]; then
  echo "runtime configuration is valid"
  exit 0
fi
[ -x "$executable" ] || { echo "DST executable is missing: $executable" >&2; exit 66; }

export LD_LIBRARY_PATH="$server_root/bin64/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
cd "$(dirname "$executable")"
exec "$executable" \
  -persistent_storage_root "$storage_root" \
  -conf_dir "$conf_dir" \
  -cluster "$cluster" \
  -shard "$shard" \
  -console
