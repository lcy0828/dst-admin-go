#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$repo"

go test ./agent ./server ./routers ./internal/agents ./internal/runtimedriver -count=1
grep -F 'USER 10000:10000' deploy/docker/Dockerfile.agent >/dev/null
grep -F '/var/lib/dst-admin/unmanaged-saves' deploy/docker/Dockerfile.control-plane >/dev/null
grep -F '/var/lib/dst-admin/unmanaged-server' deploy/docker/control-plane-entrypoint.sh >/dev/null
grep -F '/var/lib/dst-admin/unmanaged-ugc' deploy/docker/control-plane-entrypoint.sh >/dev/null
grep -F '/var/lib/dst-admin/workshop/steamapps/workshop/content/322330' deploy/docker/control-plane-entrypoint.sh >/dev/null
grep -F 'STEAM_CMD_PATH = /usr/games/steamcmd' deploy/docker/config/control-plane.conf >/dev/null
grep -F 'HOME=/var/lib/dst-admin' deploy/docker/Dockerfile.control-plane >/dev/null
grep -F 'DST_ADMIN_STEAMCMD_PATH:' deploy/docker/compose.yaml >/dev/null
key=${DST_ADMIN_AGENT_SECURITY_KEY:-MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=}
compose_config=$(mktemp)
trap 'rm -f "$compose_config"' EXIT
DST_ADMIN_AGENT_ID=smoke-agent DST_ADMIN_AGENT_SECURITY_KEY=$key docker compose -f deploy/docker/compose.yaml --profile container-control --profile dst-runtime config >"$compose_config"

agent_config=$(sed -n '/^  agent:/,/^  control-plane:/p' "$compose_config")
control_config=$(sed -n '/^  control-plane:/,/^  dst-master:/p' "$compose_config")
control_init_config=$(sed -n '/^  control-data-init:/,/^  control-plane:/p' "$compose_config")
runtime_config=$(sed -n '/^  dst-master:/,/^  volume-init:/p' "$compose_config")
volume_init_config=$(sed -n '/^  volume-init:/,/^networks:/p' "$compose_config")

assert_mount_mode() {
  block=$1
  target=$2
  expected=$3
  echo "$block" | awk -v target="$target" -v expected="$expected" '
    index($0, "target: " target) { found=1 }
    found && /read_only: true/ { mode="readonly" }
    found && /volume: \{\}/ {
      if (mode == "") mode="writable"
      if (mode != expected) exit 1
      matched=1
      exit 0
    }
    END { if (!found || !matched) exit 1 }
  '
}

assert_mount_mode "$agent_config" /srv/dst/server readonly
assert_mount_mode "$agent_config" /srv/dst/server/mods writable
echo "$agent_config" | grep -F 'hostname: smoke-agent' >/dev/null
echo "$agent_config" | grep -A1 -F -- '- -id' | grep -F -- '- smoke-agent' >/dev/null
assert_mount_mode "$runtime_config" /opt/dst/server readonly
assert_mount_mode "$runtime_config" /opt/dst/server/mods readonly
echo "$volume_init_config" | grep -F 'user: "0:0"' >/dev/null
echo "$volume_init_config" | grep -F 'install -d -o 10000 -g 10000 -m 0755 /srv/dst/server/mods' >/dev/null
echo "$volume_init_config" | grep -F 'chown -R 10000:10000 /srv/dst/saves' >/dev/null
if echo "$control_config" | grep -E '/srv/dst|docker\.sock|dst-mods' >/dev/null; then
  echo "control-plane unexpectedly received a DST or Docker mount" >&2
  exit 1
fi
echo "$control_config" | grep -F 'DST_ADMIN_AGENT_SECURITY_KEY:' >/dev/null
echo "$control_config" | grep -F 'DST_ADMIN_STEAMCMD_PATH: /usr/games/steamcmd' >/dev/null
echo "$control_config" | grep -F 'DST_ADMIN_WORKSHOP_DOWNLOAD: /var/lib/dst-admin/workshop' >/dev/null
echo "$control_config" | grep -F 'DST_ADMIN_WORKSHOP_CONTENT: /var/lib/dst-admin/workshop/steamapps/workshop/content/322330' >/dev/null
echo "$control_init_config" | grep -F 'user: "0:0"' >/dev/null
echo "$control_init_config" | grep -F 'chown -R 10001:10001 /var/lib/dst-admin' >/dev/null
echo "$control_init_config" | grep -F 'workshop/steamapps/workshop/downloads/322330' >/dev/null
if echo "$control_config" | grep -F 'DST_ADMIN_AGENT_SECURITY_KEY: ""' >/dev/null; then
  echo "control-plane received an empty Agent security key" >&2
  exit 1
fi

if [ "${DST_ADMIN_SMOKE_BUILD:-0}" = "1" ]; then
	DST_ADMIN_AGENT_SECURITY_KEY=$key docker compose -f deploy/docker/compose.yaml build control-plane agent dst-master
	docker run --rm --entrypoint /bin/sh dst-admin/control-plane:${DST_ADMIN_VERSION:-dev} -ec \
		'test "$(id -u)" = 10001 && test "$HOME" = /var/lib/dst-admin && command -v lua >/dev/null && command -v tmux >/dev/null'
	control_arch=$(docker image inspect dst-admin/control-plane:${DST_ADMIN_VERSION:-dev} --format '{{.Architecture}}')
	if [ "$control_arch" = amd64 ]; then
		docker run --rm --entrypoint /bin/sh dst-admin/control-plane:${DST_ADMIN_VERSION:-dev} -ec 'test -x /usr/games/steamcmd'
	fi
  [ "$(docker run --rm --entrypoint /usr/bin/id dst-admin/agent:${DST_ADMIN_VERSION:-dev} -u)" = "10000" ]
  [ "$(docker run --rm --entrypoint /usr/bin/id dst-admin/dst-runtime:${DST_ADMIN_VERSION:-dev} -u)" = "10000" ]
  docker run --rm \
    -e DST_CLUSTER=Smoke -e DST_SHARD=Master -e DST_CONF_DIR=DoNotStarveTogether -e DST_ADMIN_VALIDATE_ONLY=1 \
    dst-admin/dst-runtime:${DST_ADMIN_VERSION:-dev}
fi
echo "deployment smoke checks passed"
