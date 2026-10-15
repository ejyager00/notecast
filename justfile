# notecast. `just check` is the one gate; run it before every commit.

channel := env_var_or_default("NOTECAST_CHANNEL", env_var("HOME") / ".guix-channel/guix")
# Until the speech packages are pulled, resolve them from the channel checkout.
guix_shell := "guix shell -L " + channel + " -m manifest.scm --"

default:
    @just --list

# The one gate: Go format/vet/staticcheck/race tests, then Python lint/tests.
check: check-go check-py

check-go:
    {{guix_shell}} sh -c 'cd server && test -z "$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files above need formatting"; exit 1; }; go vet ./... && go tool staticcheck ./... && go test -race -count=1 ./...'

check-py:
    {{guix_shell}} sh -c 'ruff check worker eval && ruff format --check worker eval && cd worker && python3 -m pytest -q'

fmt:
    {{guix_shell}} sh -c 'cd server && gofmt -w . && cd .. && ruff format worker eval && ruff check --fix worker eval'

# Local server on loopback with the dev auth bypass. Worker token: "dev-token".
dev_env := "DATA_DIR=" + justfile_directory() / ".dev-data" + " PUBLIC_BIND=127.0.0.1:8790 WORKER_BIND=127.0.0.1:8791 PUBLIC_ORIGIN=http://127.0.0.1:8790 DEV_AUTH_EMAIL=dev@localhost WORKER_TOKEN_SHA256=c91cbbedf8c712e8e2b7517ddeca8fe4fde839ebd8339e0b2001363002b37712"

dev:
    {{guix_shell}} sh -c 'cd server && env {{dev_env}} go run ./cmd/notecast'

# Canned-result worker against `just dev`, for UI work without the GPU.
fakeworker:
    {{guix_shell}} sh -c 'cd server && NOTECAST_WORKER_TOKEN=dev-token go run ./cmd/fakeworker -url http://127.0.0.1:8791'

# Static arm64 binary for asahihome, named by commit.
build-release:
    {{guix_shell}} sh -c 'sha=$(git rev-parse --short=12 HEAD) && cd server && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$sha" -o ../dist/notecast-$sha ./cmd/notecast && echo dist/notecast-$sha'

# Download and sha256-verify pinned model weights (resumable).
fetch-models *ARGS:
    python3 worker/scripts/fetch_models.py {{ARGS}}

verify-models:
    python3 worker/scripts/fetch_models.py --verify-only

# Phase 0: OCR benchmark over $NOTECAST_EVAL_DIR/pages (default ~/notecast-eval).
eval-ocr *ARGS:
    {{guix_shell}} sh -c 'cd eval && PYTHONPATH=../worker python3 ocr_bench.py {{ARGS}}'

# Phase 0: photos -> MP3, end to end, no server.
eval-render *ARGS:
    {{guix_shell}} sh -c 'cd eval && PYTHONPATH=../worker python3 render_note.py {{ARGS}}'

# Phase 0: one narration in several voices.
eval-voices *ARGS:
    {{guix_shell}} sh -c 'cd eval && PYTHONPATH=../worker python3 voices.py {{ARGS}}'

shell:
    {{guix_shell}} bash

# Run the real worker against a server (default: `just dev`, token "dev-token").
worker CONFIG=(env_var("HOME") / ".config/notecast/worker.toml"):
    {{guix_shell}} sh -c 'cd worker && PYTHONPATH=. python3 -m notecast_worker.main --config {{CONFIG}}'

# Build, ship to asahihome, repoint `current`, restart, and health-check.
deploy: build-release
    #!/usr/bin/env sh
    set -eu
    bin=$(ls -t dist/notecast-* | head -1); name=$(basename "$bin")
    scp "$bin" asahihome:opt/notecast/releases/
    ssh asahihome "chmod +x ~/opt/notecast/releases/$name && ln -sfn ~/opt/notecast/releases/$name ~/opt/notecast/releases/current && systemctl --user restart notecast && sleep 2 && systemctl --user is-active notecast && curl -sf http://100.97.104.119:8790/healthz"
    echo "deployed $name"
