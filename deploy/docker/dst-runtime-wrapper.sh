#!/bin/sh
set -eu

cluster=${DST_CLUSTER:-}
shard=${DST_SHARD:-}
storage_root=${DST_STORAGE_ROOT:-/opt/dst/saves}
conf_dir=${DST_CONF_DIR:-DoNotStarveTogether}
server_root=${DST_SERVER_ROOT:-/opt/dst/server}
executable=${DST_EXECUTABLE:-$server_root/bin64/dontstarve_dedicated_server_nullrenderer_x64}
# SteamCMD owns the shared Workshop directory. Each world keeps only its own
# Steam bookkeeping here and loads Mod content through server/mods.
ugc_directory=$storage_root/$conf_dir/.dst-admin/runtime/workshop/$cluster/$shard
runtime_mode=${DST_RUNTIME_MODE:-game}
state_dir=${DST_RUNTIME_STATE_DIR:-/run/dst-admin}
exit_status_file=$state_dir/runtime-exit-status
launch_options_file=${DST_LAUNCH_OPTIONS_FILE:-$storage_root/$conf_dir/$cluster/$shard/save/mod_config_data/dst-admin/launch-options}

case "$cluster:$shard:$conf_dir" in
  *[!A-Za-z0-9_.:-]*) echo "cluster, shard or conf dir contains unsafe characters" >&2; exit 64 ;;
esac
case "$runtime_mode" in
  game) runtime_args="-lua_vm_type=game" ;;
  luajit-jit-off) runtime_args="-lua_vm_type=jit -luajit_enabled_jit=false" ;;
  luajit-jit-on) runtime_args="-lua_vm_type=jit -luajit_enabled_jit=true" ;;
  arena-gc) runtime_args="-lua_vm_type=jit_gen -luajit_enabled_jit=false" ;;
  *) echo "DST_RUNTIME_MODE is invalid" >&2; exit 64 ;;
esac
[ -n "$cluster" ] && [ -n "$shard" ] && [ -n "$conf_dir" ] || { echo "DST_CLUSTER, DST_SHARD and DST_CONF_DIR are required" >&2; exit 64; }
case "$storage_root:$server_root:$executable" in
  *"
"*|*""*) echo "runtime path contains a newline" >&2; exit 64 ;;
esac
if [ -n "$ugc_directory" ]; then
  case "$ugc_directory" in
    /*) ;;
    *) echo "UGC directory must be absolute" >&2; exit 64 ;;
  esac
fi
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
mkdir -p "$ugc_directory"
set -- "$executable" \
  -persistent_storage_root "$storage_root" \
  -conf_dir "$conf_dir" \
  -cluster "$cluster" \
  -shard "$shard"
if [ -n "$ugc_directory" ]; then
  set -- "$@" -ugc_directory "$ugc_directory"
fi
set -- "$@" $runtime_args -skip_update_server_mods
rm -f "$launch_options_file"
set +e
"$@"
runtime_status=$?
set -e

status_tmp=$exit_status_file.$$
printf '%s\n' "$runtime_status" > "$status_tmp"
mv "$status_tmp" "$exit_status_file"
exit "$runtime_status"
