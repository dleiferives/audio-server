#!/usr/bin/env bash

set -Eeuo pipefail

GO_VERSION="1.26.7"
GO_ARCHIVE="go${GO_VERSION}.linux-amd64.tar.gz"
GO_SHA256="ffb5f8de10c62550dfddab66b36b57030721e0a44a3218e9e1181d7b59f121ca"
GO_URL="https://go.dev/dl/${GO_ARCHIVE}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
DOWNLOAD_DIR="$(mktemp -d /tmp/audio-server-setup.XXXXXX)"
SERVER_PID=""

cleanup() {
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  rm -rf -- "${DOWNLOAD_DIR}"
}
trap cleanup EXIT

if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
  echo "This setup script currently supports Linux x86-64 only." >&2
  exit 1
fi

echo "==> Installing required Debian packages"
sudo apt-get update
sudo apt-get install -y ca-certificates curl git espeak-ng ffmpeg

echo "==> Downloading Go ${GO_VERSION} from go.dev"
curl --fail --location --show-error \
  --output "${DOWNLOAD_DIR}/${GO_ARCHIVE}" \
  "${GO_URL}"

echo "${GO_SHA256}  ${DOWNLOAD_DIR}/${GO_ARCHIVE}" | sha256sum --check --strict

echo "==> Installing Go ${GO_VERSION} in /usr/local/go"
sudo rm -rf -- /usr/local/go
sudo tar -C /usr/local -xzf "${DOWNLOAD_DIR}/${GO_ARCHIVE}"
sudo ln -sfn /usr/local/go/bin/go /usr/local/bin/go
sudo ln -sfn /usr/local/go/bin/gofmt /usr/local/bin/gofmt

export PATH="/usr/local/go/bin:${PATH}"
hash -r

echo "==> Tool versions"
go version
espeak-ng --version | head -n 1
ffmpeg -version | head -n 1

echo "==> Initializing the required MFA-go submodule"
git -C "${PROJECT_DIR}" \
  -c submodule.mfa-go.url=https://github.com/dleiferives/MFA-go.git \
  submodule update --init mfa-go

echo "==> Building audio-server"
make -C "${PROJECT_DIR}" build

echo "==> Running Go tests"
go -C "${PROJECT_DIR}" test ./...

echo "==> Smoke-testing the minimal local server"
(
  cd "${PROJECT_DIR}"
  ./bin/audio-server -config=tmp/scripts/config.local.yml
) >"${DOWNLOAD_DIR}/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 30); do
  if curl --fail --silent --show-error \
    http://127.0.0.1:18010/healthz \
    >"${DOWNLOAD_DIR}/health.json"; then
    break
  fi
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    cat "${DOWNLOAD_DIR}/server.log" >&2
    exit 1
  fi
  sleep 0.25
done

curl --fail --silent --show-error \
  http://127.0.0.1:18010/v1/audio/speech \
  -H 'Content-Type: application/json' \
  -d '{"input":"local setup works","language":"en","voice":"auto","response_format":"wav"}' \
  --output "${DOWNLOAD_DIR}/smoke-test.wav"
test -s "${DOWNLOAD_DIR}/smoke-test.wav"

kill "${SERVER_PID}"
wait "${SERVER_PID}" 2>/dev/null || true
SERVER_PID=""

cat <<'EOF'

Local setup completed successfully.

Start the minimal local server with:
  cd /home/dleiferives/projs/audio-server
  ./bin/audio-server -config=tmp/scripts/config.local.yml

Then open http://127.0.0.1:18010 or check:
  curl -sS http://127.0.0.1:18010/healthz
EOF
