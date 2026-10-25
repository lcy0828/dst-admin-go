#!/usr/bin/env bash
set -euo pipefail

# Use a dedicated upstream checkout; this applies only the reviewed build patches.
recipe_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source_dir="$(cd "${1:?usage: build.sh UPSTREAM_CHECKOUT OUTPUT_ZIP}" && pwd)"
output_zip=${2:?output ZIP is required}
image_name=dst-admin-luajit-builder:debian12

read_pin() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' \
    "$recipe_dir/source.json" "$1"
}
check_commit() {
  local actual
  actual=$(git -C "$source_dir/$1" rev-parse HEAD)
  if [[ "$actual" != "$(read_pin "$2")" ]]; then
    echo "Unexpected source commit at $1: $actual" >&2
    exit 1
  fi
}
check_commit . commit
check_commit luajit luajitCommit
check_commit vcpkg vcpkgCommit
check_commit 3rd/frida-gum-src fridaCommit

apply_patch_once() {
  local checkout=$1 patch=$2
  if git -C "$checkout" apply --reverse --check "$patch" 2>/dev/null; then
    return
  fi
  git -C "$checkout" diff --exit-code --quiet
  git -C "$checkout" apply --check "$patch"
  git -C "$checkout" apply "$patch"
}
apply_patch_once "$source_dir" "$recipe_dir/debian12.patch"
apply_patch_once "$source_dir/luajit" "$recipe_dir/luajit-source-date.patch"

docker build --platform linux/amd64 -t "$image_name" "$recipe_dir"
docker run --rm --platform linux/amd64 \
  -e CI=1 -e GITHUB_ACTIONS=1 \
  -e "SOURCE_DATE_EPOCH=$(read_pin sourceDateEpoch)" \
  -e "CMAKE_BUILD_PARALLEL_LEVEL=${CMAKE_BUILD_PARALLEL_LEVEL:-4}" \
  -e VCPKG_DEFAULT_BINARY_CACHE=/workspace/.vcpkg-bincache \
  -e 'VCPKG_BINARY_SOURCES=clear;files,/workspace/.vcpkg-bincache,readwrite' \
  -v "$source_dir:/workspace" -w /workspace "$image_name" bash -lc '
    set -euo pipefail
    mkdir -p .vcpkg-bincache
    ./vcpkg/bootstrap-vcpkg.sh -disableMetrics
    cmake --preset ninja-multi-vcpkg
    cmake --build builds/ninja-multi-vcpkg --config RelWithDebInfo
    cmake --build builds/ninja-multi-vcpkg --config RelWithDebInfo --target install
    LUA_BIN=/workspace/builds/ninja-multi-vcpkg/luajit/RelWithDebInfo/luajit \
      ctest --test-dir builds/ninja-multi-vcpkg -C RelWithDebInfo \
        --output-on-failure --timeout 60 \
        -E "^(plugin_dedicated_sim_pause|plugin_client_inject_smoke|plugin_client_host_smoke|game_performance_.*|game_mod_throw_abort)$"
  '
python3 "$recipe_dir/package.py" --source "$source_dir" --output "$output_zip"
