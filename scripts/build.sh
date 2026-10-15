#!/bin/bash
# Builds hivemind and hivemind-server into <repo>/bin/.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

# Everything is pure Go (modernc.org/sqlite needs no cgo), and
# disabling cgo keeps the build independent of the host C compiler.
export CGO_ENABLED=0
go build -o bin/hivemind ./cmd/hivemind
go build -o bin/hivemind-server ./cmd/hivemind-server
echo "Built bin/hivemind and bin/hivemind-server"
