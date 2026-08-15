#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
cd "$repo"

go test ./agent ./server ./routers ./internal/agents ./internal/runtimedriver -count=1
key=${DST_ADMIN_AGENT_SECURITY_KEY:-MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=}
DST_ADMIN_AGENT_SECURITY_KEY=$key docker compose -f deploy/docker/compose.yaml config >/dev/null

if [ "${DST_ADMIN_SMOKE_BUILD:-0}" = "1" ]; then
  DST_ADMIN_AGENT_SECURITY_KEY=$key docker compose -f deploy/docker/compose.yaml build control-plane agent dst-master
  docker run --rm \
    -e DST_CLUSTER=Smoke -e DST_SHARD=Master -e DST_CONF_DIR=DoNotStarveTogether -e DST_ADMIN_VALIDATE_ONLY=1 \
    dst-admin/dst-runtime:${DST_ADMIN_VERSION:-dev}
fi
echo "deployment smoke checks passed"
