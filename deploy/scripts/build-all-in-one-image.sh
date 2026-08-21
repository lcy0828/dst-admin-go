#!/bin/sh
set -eu

usage() {
  echo "usage: $0 [--frontend PATH] [--tag IMAGE] [--debian-mirror URL] [--debian-security-mirror URL]" >&2
  exit 64
}

script_dir=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
backend_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
frontend_root=$(CDPATH= cd -- "$backend_root/../dst-admin-vue-v3" 2>/dev/null && pwd || true)
image=dst-admin/all-in-one:dev
debian_mirror=${DST_ADMIN_DEBIAN_MIRROR:-http://deb.debian.org/debian}
debian_security_mirror=${DST_ADMIN_DEBIAN_SECURITY_MIRROR:-http://deb.debian.org/debian-security}
while [ "$#" -gt 0 ]; do
  case "$1" in
    --frontend) [ "$#" -ge 2 ] || usage; frontend_root=$2; shift 2 ;;
    --tag) [ "$#" -ge 2 ] || usage; image=$2; shift 2 ;;
    --debian-mirror) [ "$#" -ge 2 ] || usage; debian_mirror=$2; shift 2 ;;
    --debian-security-mirror) [ "$#" -ge 2 ] || usage; debian_security_mirror=$2; shift 2 ;;
    *) usage ;;
  esac
done

[ -f "$backend_root/go.mod" ] || { echo "backend repository is unavailable" >&2; exit 66; }
[ -f "$frontend_root/package.json" ] || { echo "frontend repository is unavailable; pass --frontend PATH" >&2; exit 66; }
[ -f "$frontend_root/package-lock.json" ] || { echo "frontend package-lock.json is required" >&2; exit 66; }

build_context=$(mktemp -d "${TMPDIR:-/tmp}/dst-admin-all-in-one.XXXXXX")
cleanup() {
  rm -rf "$build_context"
}
trap cleanup EXIT INT TERM
install -d "$build_context/backend" "$build_context/frontend"
tar -C "$backend_root" --exclude=.git --exclude=.DS_Store --exclude='*.db' --exclude='*.db-shm' --exclude='*.db-wal' -cf - . |
  tar -C "$build_context/backend" -xf -
tar -C "$frontend_root" --exclude=.git --exclude=node_modules --exclude=dist --exclude=.DS_Store -cf - . |
  tar -C "$build_context/frontend" -xf -

docker build \
  --platform linux/amd64 \
  --progress plain \
  --build-arg "DEBIAN_MIRROR=$debian_mirror" \
  --build-arg "DEBIAN_SECURITY_MIRROR=$debian_security_mirror" \
  --file "$backend_root/deploy/docker/Dockerfile.all-in-one" \
  --tag "$image" \
  "$build_context"
echo "built $image"
