#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

if [[ "$(uname -s)" != "Linux" ]]; then
  echo "This setup script currently supports Linux only." >&2
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "==> Go is missing; running the local Go/runtime setup first"
  "${SCRIPT_DIR}/setup-local.sh"
fi

if ! command -v nvcc >/dev/null 2>&1; then
  cat >&2 <<'EOF'
CUDA's nvcc compiler is required but was not found.
Install the CUDA Toolkit for your distribution from:
  https://developer.nvidia.com/cuda-downloads
Then rerun this script. The NVIDIA display driver alone does not include nvcc.
EOF
  exit 1
fi
CUDA_ROOT="$(dirname -- "$(dirname -- "$(readlink -f -- "$(command -v nvcc)")")")"

echo "==> Installing native build and media tools"
sudo apt-get update
sudo apt-get install -y \
  build-essential ca-certificates cmake curl ffmpeg git ninja-build pkg-config

echo "==> Initializing native model runtimes"
git -C "${PROJECT_DIR}" submodule update --init audio.cpp transcribe.cpp wespeaker

echo "==> Building the full CUDA audio.cpp runtime"
make -C "${PROJECT_DIR}" CUDA_HOME="${CUDA_ROOT}" build-audiocpp

echo "==> Building transcribe.cpp and the WeSpeaker ONNX service"
make -C "${PROJECT_DIR}" CUDA_HOME="${CUDA_ROOT}" build-transcribecpp build-wespeaker

echo "==> Downloading pinned analysis and transcription models"
make -C "${PROJECT_DIR}" \
  download-sortformer download-bs-roformer download-wespeaker download-cohere download-voxtral

echo "==> Building and testing audio-server"
make -C "${PROJECT_DIR}" build
go -C "${PROJECT_DIR}" test ./...

cat <<EOF

Autodubbing analysis setup is complete.

Start the server:
  cd ${PROJECT_DIR}
  make run

Submit diarization, captions, overlap detection, and speaker embeddings:
  curl -sS http://127.0.0.1:8010/v1/audio/analysis-jobs \
    -F file=@episode.mkv \
    -F transcription_model=cohere-transcribe \
    -F language=en

Poll the returned status_url, then fetch result_url.
EOF
