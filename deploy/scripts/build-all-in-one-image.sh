#!/bin/sh
set -eu
script_dir=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
exec node "$script_dir/build-image.mjs" --kind all-in-one "$@"
