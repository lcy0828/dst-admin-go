#!/bin/sh
set -eu

usage() {
  echo "usage: $0 --root PATH (--release DIRECTORY_NAME | --rollback)" >&2
  exit 64
}
root=""
release=""
rollback=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --root) [ "$#" -ge 2 ] || usage; root=$2; shift 2 ;;
    --release) [ "$#" -ge 2 ] || usage; release=$2; shift 2 ;;
    --rollback) rollback=true; shift ;;
    *) usage ;;
  esac
done
case "$root" in /*) ;; *) usage ;; esac
[ "$root" != / ] && [ -d "$root/releases" ] || usage
root=$(CDPATH= cd -- "$root" && pwd -P)
for link in current previous; do
  if [ -e "$root/$link" ] && [ ! -L "$root/$link" ]; then
    echo "$root/$link must be a release symlink" >&2
    exit 65
  fi
done
if [ "$rollback" = true ]; then
  [ -z "$release" ] && [ -L "$root/previous" ] || usage
  release=$(readlink "$root/previous")
  case "$release" in releases/*) release=${release#releases/} ;; *) usage ;; esac
fi
case "$release" in ""|*[!A-Za-z0-9._+-]*|.|..) usage ;; esac
selected="$root/releases/$release"
[ ! -L "$selected" ] && [ -d "$selected" ] && [ -f "$selected/SHA256SUMS" ] && [ -x "$selected/dst-admin" ] && [ -f "$selected/public/index.html" ] || {
  echo "release is incomplete: $selected" >&2
  exit 66
}
(
  cd "$selected"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum --quiet -c SHA256SUMS
  else
    shasum -a 256 -c SHA256SUMS >/dev/null
  fi
)
old=""
if [ -L "$root/current" ]; then
  old=$(readlink "$root/current")
  case "$old" in releases/*) ;; *) echo "current release points outside releases" >&2; exit 65 ;; esac
fi
[ "$old" != "releases/$release" ] || { echo "already active: $release"; exit 0; }
switch_link() {
  destination=$1
  value=$2
  temporary=$(mktemp -d "$root/.release-link.XXXXXX")
  ln -s "$value" "$temporary/link"
  case "$(uname -s)" in
    Darwin) mv -fh "$temporary/link" "$destination" ;;
    *) mv -fT "$temporary/link" "$destination" ;;
  esac
  rmdir "$temporary"
}
if [ -n "$old" ]; then switch_link "$root/previous" "$old"; fi
switch_link "$root/current" "releases/$release"
echo "active: $release; configuration, database, saves and game processes were not changed"
