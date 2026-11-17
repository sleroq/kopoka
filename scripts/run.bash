#!/usr/bin/env bash

set -e

SCRIPT=$(readlink -f "$0")
SCRIPTPATH=$(dirname "$SCRIPT")
cd "$(dirname "$SCRIPTPATH")"

source ./scripts/env.bash

if [[ -z "${KOPOKA_RENDERER:-}" ]]; then
  cargo build --bin kopoka-render
  export KOPOKA_RENDERER="$PWD/target/debug/kopoka-render"
fi

go run ./src
