#!/bin/sh
set -eu

runtime_dir=/run/dst-admin/tmux
socket=${DST_CONSOLE_SOCKET:-$runtime_dir/tmux.sock}
session=${DST_CONSOLE_SESSION:-dst}
pane="=$session:0.0"
instance_id="$(cat /proc/sys/kernel/random/uuid)"

case "$session" in
  ""|*[!A-Za-z0-9._-]*) echo "invalid tmux session" >&2; exit 64 ;;
esac
case "$socket" in
  /*) ;;
  *) echo "tmux socket must be absolute" >&2; exit 64 ;;
esac

mkdir -p "$runtime_dir"
chmod 0700 "$runtime_dir"
printf '%s\n' "$instance_id" > /run/dst-admin/instance-id
chmod 0600 /run/dst-admin/instance-id

shutdown_requested=0
request_shutdown() {
  shutdown_requested=1
  if tmux -S "$socket" has-session -t "=$session" 2>/dev/null; then
    tmux -S "$socket" send-keys -t "$pane" -l -- 'c_shutdown(true)' \; send-keys -t "$pane" Enter || true
  fi
}
trap request_shutdown TERM INT

tmux -S "$socket" new-session -d -s "$session" -n runtime sleep 86400
tmux -S "$socket" set-window-option -t "$session:0" remain-on-exit on
tmux -S "$socket" respawn-pane -k -t "$pane" /usr/local/bin/dst-runtime-wrapper
echo "DST runtime ready: instance=$instance_id session=$session"

while [ "$(tmux -S "$socket" display-message -p -t "$pane" '#{pane_dead}')" = "0" ]; do
  sleep 1 &
  wait $! || true
done

status="$(tmux -S "$socket" display-message -p -t "$pane" '#{pane_dead_status}')"
tmux -S "$socket" capture-pane -p -S -200 -t "$pane" || true
tmux -S "$socket" kill-session -t "=$session" || true
if [ "$shutdown_requested" = "1" ] && [ "$status" = "143" ]; then
  status=0
fi
exit "$status"
