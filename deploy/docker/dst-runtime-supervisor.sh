#!/bin/sh
set -eu

state_dir=${DST_RUNTIME_STATE_DIR:-/run/dst-admin}
runtime_dir=$state_dir/tmux
socket=${DST_CONSOLE_SOCKET:-$runtime_dir/tmux.sock}
session=${DST_CONSOLE_SESSION:-dst}
pane="=$session:0.0"
status_retry_limit=${DST_SUPERVISOR_STATUS_RETRY_LIMIT:-10}
status_retry_delay=${DST_SUPERVISOR_STATUS_RETRY_DELAY:-0.1}
exit_status_file=$state_dir/runtime-exit-status

if [ -n "${DST_RUNTIME_INSTANCE_ID:-}" ]; then
  instance_id=$DST_RUNTIME_INSTANCE_ID
else
  instance_id="$(cat /proc/sys/kernel/random/uuid)"
fi

case "$session" in
  ""|*[!A-Za-z0-9._-]*) echo "invalid tmux session" >&2; exit 64 ;;
esac
case "$socket" in
  /*) ;;
  *) echo "tmux socket must be absolute" >&2; exit 64 ;;
esac

mkdir -p "$runtime_dir"
chmod 0700 "$runtime_dir"
printf '%s\n' "$instance_id" > "$state_dir/instance-id"
chmod 0600 "$state_dir/instance-id"
rm -f "$exit_status_file"

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

status=
status_attempt=0
if [ -r "$exit_status_file" ]; then
  candidate="$(cat "$exit_status_file")"
  case "$candidate" in
    ""|*[!0-9]*) ;;
    *) status=$candidate ;;
  esac
fi
while [ "$status_attempt" -lt "$status_retry_limit" ]; do
  if [ -n "$status" ]; then
    break
  fi
  candidate=
  if candidate="$(tmux -S "$socket" display-message -p -t "$pane" '#{pane_dead_status}' 2>/dev/null)"; then
    case "$candidate" in
      ""|*[!0-9]*) ;;
      *) status=$candidate; break ;;
    esac
  fi
  status_attempt=$((status_attempt + 1))
  if [ "$status_attempt" -lt "$status_retry_limit" ]; then
    sleep "$status_retry_delay"
  fi
done

tmux -S "$socket" capture-pane -p -S -200 -t "$pane" || true
tmux -S "$socket" kill-session -t "=$session" || true
if [ -z "$status" ]; then
  echo "unable to read DST runtime exit status from wrapper or tmux after $status_attempt attempts" >&2
  status=70
fi
if [ "$shutdown_requested" = "1" ] && [ "$status" = "143" ]; then
  status=0
fi
exit "$status"
