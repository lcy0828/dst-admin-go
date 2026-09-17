#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$repo"

if command -v go >/dev/null 2>&1; then
  go test ./agent ./server ./routers ./internal/agents ./internal/shards ./internal/worldstate ./internal/runtimedriver ./internal/deploymentprofile ./internal/topology ./internal/roomprovision ./internal/distributedbackup ./internal/gameupdate -count=1
else
  docker run --rm \
    --mount "type=bind,src=$repo,dst=/src,readonly" \
    --workdir /src \
    golang:1.25-bookworm \
    go test ./agent ./server ./routers ./internal/agents ./internal/shards ./internal/worldstate ./internal/runtimedriver ./internal/deploymentprofile ./internal/topology ./internal/roomprovision ./internal/distributedbackup ./internal/gameupdate -count=1
fi

sh -n \
  deploy/docker/all-in-one-entrypoint.sh \
  deploy/docker/dst-runtime-supervisor.sh \
  deploy/docker/dst-runtime-wrapper.sh \
  deploy/scripts/install-native-agent.sh \
  deploy/scripts/install-native-local.sh
grep -F 'COPY --from=docker-cli /usr/local/bin/docker' deploy/docker/Dockerfile.all-in-one >/dev/null
grep -F 'USER 10000:10000' deploy/docker/Dockerfile.dst-runtime >/dev/null
grep -F 'install -o "$service_user" -g "$service_group" -m 0600 "$config" /var/lib/dst-admin-agent/agent.conf' deploy/scripts/install-native-agent.sh >/dev/null
grep -F 'ExecStart=/var/lib/dst-admin-agent/bin/dst-admin-agent -config /var/lib/dst-admin-agent/agent.conf' deploy/systemd/dst-admin-agent.service >/dev/null
grep -F 'KillMode=process' deploy/systemd/dst-admin-agent.service >/dev/null
grep -F 'KillMode=process' deploy/systemd/dst-admin-local.service >/dev/null
grep -F 'owner.lock' internal/shards/native_owner_lock_unix.go >/dev/null
grep -F 'RUNTIME_OWNER_CONFLICT' internal/shards/native_owner_lock_unix.go >/dev/null

default_services=$(docker compose -f deploy/docker/compose.yaml config --services)
[ "$default_services" = "dst-admin" ] || {
  echo "default Compose must only start dst-admin; got: $default_services" >&2
  exit 1
}
build_services=$(docker compose -f deploy/docker/compose.yaml --profile build config --services)
printf '%s\n' "$build_services" | grep -Fx 'dst-admin' >/dev/null
printf '%s\n' "$build_services" | grep -Fx 'dst-runtime-image' >/dev/null
docker compose -f deploy/docker/compose.yaml --profile build config |
  awk '
    /^  dst-runtime-image:/ { runtime = 1; next }
    runtime && /^  [^ ]/ { exit }
    runtime && /platform: linux\/amd64/ { found = 1 }
    END { exit !found }
  '

compose_config=$(mktemp)
smoke_volume=
cleanup() {
  rm -f "$compose_config"
  [ -z "$smoke_volume" ] || docker volume rm "$smoke_volume" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
docker compose -f deploy/docker/compose.yaml config >"$compose_config"
grep -F 'DST_ADMIN_LOCAL_RUNTIME_DRIVER: container' "$compose_config" >/dev/null
grep -F 'DST_ADMIN_LOCAL_CONTAINER_IMAGE: dst-admin/dst-runtime:dev' "$compose_config" >/dev/null
grep -F 'DST_ADMIN_LOCAL_CONTAINER_SAVE_SOURCE: /opt/dst/saves' "$compose_config" >/dev/null
grep -F 'DST_ADMIN_LOCAL_CONTAINER_SERVER_SOURCE: /opt/dst/server' "$compose_config" >/dev/null
grep -F 'DST_ADMIN_LOCAL_CONTAINER_UGC_SOURCE: /opt/dst/workshop/steamapps/workshop' "$compose_config" >/dev/null
grep -F 'source: /var/run/docker.sock' "$compose_config" >/dev/null
grep -F 'source: /opt/dst' "$compose_config" >/dev/null
if grep -Eq '^  (agent|dst-master|dst-caves):' "$compose_config"; then
  echo "default Compose unexpectedly contains a local Agent or static Shard" >&2
  exit 1
fi

if [ "${DST_ADMIN_SMOKE_BUILD:-0}" = "1" ]; then
  runtime_image=${DST_ADMIN_RUNTIME_IMAGE:-dst-admin/dst-runtime:dev}
  admin_image=${DST_ADMIN_IMAGE:-dst-admin/all-in-one:dev}
  docker compose -f deploy/docker/compose.yaml --profile build build dst-runtime-image
  deploy/scripts/build-all-in-one-image.sh --tag "$admin_image"
  [ "$(docker image inspect "$runtime_image" --format '{{.Architecture}}')" = "amd64" ]
  [ "$(docker run --rm --platform linux/amd64 --entrypoint /usr/bin/id "$runtime_image" -u)" = "10000" ]
  docker run --rm --platform linux/amd64 --entrypoint /usr/local/bin/dst-runtime-wrapper \
    -e DST_ADMIN_VALIDATE_ONLY=1 \
    -e DST_CLUSTER=Smoke \
    -e DST_SHARD=Master \
    -e DST_CONF_DIR=. \
    -e DST_UGC_DIRECTORY=/opt/dst/workshop/steamapps/workshop \
    "$runtime_image" >/dev/null
  docker run --rm --platform linux/amd64 --entrypoint /bin/sh "$admin_image" -ec \
    'command -v docker >/dev/null && test -x /usr/games/steamcmd && command -v lua5.1 >/dev/null && command -v tmux >/dev/null'
  smoke_volume="dst-admin-smoke-data-$$"
  docker run --rm --platform linux/amd64 \
    --mount "type=volume,src=$smoke_volume,dst=/opt/dst" \
    --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    -e DST_ADMIN_BOOTSTRAP_DST=false \
    "$admin_image" /bin/sh -ec \
    'test "$(id -u)" = 10000; test "$(id -g)" = "$(stat -c "%g" /var/run/docker.sock)"; docker info --format "{{.ServerVersion}}" >/dev/null'
  docker volume rm "$smoke_volume" >/dev/null
  smoke_volume=
fi

echo "deployment smoke checks passed"
