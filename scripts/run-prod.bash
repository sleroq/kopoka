#!/usr/bin/env bash

set -e

SCRIPT=$(readlink -f "$0")
SCRIPTPATH=$(dirname "$SCRIPT")
cd "$(dirname "$SCRIPTPATH")"

source ./scripts/env.bash

export KOPOKA_RENDERER="${KOPOKA_RENDERER:-$PWD/dist/kopoka-render}"
./dist/kopoka
