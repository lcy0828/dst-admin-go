#!/bin/sh
set -eu
umask 077

if [ "$(id -u)" = "0" ]; then
  for directory in control saves server workshop backups maps
  do
    install -d -o 10000 -g 10000 -m 0700 "/data/$directory"
  done
  marker=/data/control/.all-in-one-layout-v1
  if [ ! -e "$marker" ]; then
    chown -R 10000:10000 /data
    install -o 10000 -g 10000 -m 0600 /dev/null "$marker"
  fi
  exec gosu dstadmin "$0" "$@"
fi

config=${DST_ADMIN_CONFIG:-/data/control/app.conf}
if [ ! -e "$config" ]; then
  cp /usr/local/share/dst-admin/all-in-one.conf "$config"
  chmod 0600 "$config"
fi

server_binary=/data/server/bin64/dontstarve_dedicated_server_nullrenderer_x64
if [ "${DST_ADMIN_BOOTSTRAP_DST:-true}" = "true" ] && [ ! -x "$server_binary" ]; then
  if [ ! -x /usr/games/steamcmd ]; then
    echo "SteamCMD is unavailable on this architecture; mount a prepared DST server at /data/server" >&2
    exit 69
  fi
  /usr/games/steamcmd \
    +force_install_dir /data/server \
    +login anonymous \
    +app_update 343050 validate \
    +quit
  [ -x "$server_binary" ] || { echo "DST dedicated server installation did not produce $server_binary" >&2; exit 70; }
fi

managed_panes() {
  tmux list-panes -a -F '#{pane_id} #{pane_current_command}' 2>/dev/null |
    awk '$2 ~ /^dontstarve/ { print $1 }'
}

shutdown_worlds() {
  panes=$(managed_panes || true)
  [ -n "$panes" ] || return 0
  echo "saving and stopping managed DST Shards"
  for pane in $panes
  do
    case "$pane" in
      %*[!0-9]*|%) continue ;;
    esac
    tmux send-keys -l -t "$pane" -- 'c_save(); c_shutdown(true)' || true
    tmux send-keys -t "$pane" Enter || true
  done
  deadline=$(( $(date +%s) + 45 ))
  while [ "$(date +%s)" -lt "$deadline" ]
  do
    [ -z "$(managed_panes || true)" ] && return 0
    sleep 1
  done
  echo "timed out waiting for DST Shards to stop; container runtime will terminate remaining processes" >&2
}

shutting_down=false
app_pid=""
handle_signal() {
  shutting_down=true
  shutdown_worlds
  [ -z "$app_pid" ] || kill -TERM "$app_pid" 2>/dev/null || true
}
trap handle_signal TERM INT

"$@" &
app_pid=$!
set +e
wait "$app_pid"
status=$?
set -e
if [ "$shutting_down" = "true" ]; then
  set +e
  wait "$app_pid" 2>/dev/null
  status=$?
  set -e
else
  shutdown_worlds
fi
exit "$status"
