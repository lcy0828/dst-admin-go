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
smoke_projects=""
cleanup() {
  rm -f "$compose_config"
  for smoke_project in $smoke_projects; do
    DST_ADMIN_AGENT_SECURITY_KEY=$key docker compose \
      -p "$smoke_project" \
      -f deploy/docker/compose.yaml \
      --profile container-control \
      --profile dst-runtime \
      down --volumes --remove-orphans >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT HUP INT TERM
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

  smoke_base=${DST_ADMIN_SMOKE_PROJECT:-dst-admin-smoke-$$}
  for smoke_case in first legacy; do
    smoke_project="$smoke_base-$smoke_case"
    smoke_projects="$smoke_projects $smoke_project"
    compose() {
      DST_ADMIN_AGENT_ID=smoke-agent \
        DST_ADMIN_AGENT_SECURITY_KEY=$key \
        docker compose \
        -p "$smoke_project" \
        -f deploy/docker/compose.yaml \
        --profile container-control \
        --profile dst-runtime \
        "$@"
    }

    if [ "$smoke_case" = legacy ]; then
      compose run --rm --no-deps --user 0:0 --entrypoint /bin/sh control-data-init -ec '
        mkdir -p /var/lib/dst-admin/workshop/steamapps/workshop/content/322330
        mkdir -p /var/lib/dst-admin/workshop/steamapps/workshop/downloads/322330
        touch /var/lib/dst-admin/go-dont.db /var/lib/dst-admin/workshop/legacy-root-file
        chown -R 0:0 /var/lib/dst-admin
      '
      compose run --rm --no-deps --user 0:0 --entrypoint /bin/sh volume-init -ec '
        mkdir -p /var/lib/dst-admin-agent /srv/dst/saves /srv/dst/server/mods /srv/dst/mods /srv/dst/workshop
        touch /var/lib/dst-admin-agent/legacy-root-file /srv/dst/saves/legacy-root-file /srv/dst/workshop/legacy-root-file
        printf legacy-setup > /srv/dst/server/mods/dedicated_server_mods_setup.lua
        chown -R 0:0 /var/lib/dst-admin-agent /srv/dst/saves /srv/dst/server /srv/dst/mods /srv/dst/workshop
      '
    fi

    compose run --rm --no-deps control-data-init
    compose run --rm --no-deps volume-init

    # Exercise the real non-root entrypoints against initialized volumes.
    compose run --rm --no-deps control-plane /bin/sh -ec '
      test "$(id -u)" = 10001
      test -f /var/lib/dst-admin/app.conf
      test "$(stat -c %a /var/lib/dst-admin/app.conf)" = 600
      test -w /var/lib/dst-admin
      test -w /var/lib/dst-admin/workshop/steamapps/workshop/content/322330
      test -w /var/lib/dst-admin/workshop/steamapps/workshop/downloads/322330
      test "$(stat -c %u /var/lib/dst-admin)" = 10001
      test "$(stat -c %u /var/lib/dst-admin/workshop)" = 10001
    '
    compose run --rm --no-deps --entrypoint /bin/sh agent -ec '
      test "$(id -u)" = 10000
      test -w /var/lib/dst-admin-agent
      test -w /srv/dst/saves
      test -w /srv/dst/server/mods
      test -w /srv/dst/workshop
      test "$(stat -c %u /var/lib/dst-admin-agent)" = 10000
      test "$(stat -c %u /srv/dst/saves)" = 10000
      test "$(stat -c %u /srv/dst/server/mods)" = 10000
      test "$(stat -c %u /srv/dst/workshop)" = 10000
    '
    if [ "$smoke_case" = legacy ]; then
      compose run --rm --no-deps --entrypoint /bin/sh agent -ec '
        test -f /srv/dst/server/mods/dedicated_server_mods_setup.lua
        test "$(cat /srv/dst/server/mods/dedicated_server_mods_setup.lua)" = legacy-setup
      '
    fi

    # Marker-based migrations must remain harmless on every later start.
    compose run --rm --no-deps control-data-init
    compose run --rm --no-deps volume-init
    compose run --rm --no-deps control-plane /bin/sh -ec 'test -w /var/lib/dst-admin && test -f /var/lib/dst-admin/app.conf'
    compose run --rm --no-deps --entrypoint /bin/sh agent -ec 'test -w /var/lib/dst-admin-agent && test -w /srv/dst/server/mods'
    compose run --rm --no-deps \
      -e DST_CLUSTER=Smoke \
      -e DST_SHARD=Master \
      -e DST_CONF_DIR=DoNotStarveTogether \
      -e DST_ADMIN_VALIDATE_ONLY=1 \
      dst-master

    compose down --volumes --remove-orphans >/dev/null
  done
fi
echo "deployment smoke checks passed"
