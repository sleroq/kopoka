#!/usr/bin/env bash

set -e

SCRIPT=$(readlink -f "$0")
SCRIPTPATH=$(dirname "$SCRIPT")

cd "$(dirname "$SCRIPTPATH")"

cargo build --release --bin kopoka-render
go build -o dist/kopoka ./src
cp target/release/kopoka-render dist/kopoka-render
