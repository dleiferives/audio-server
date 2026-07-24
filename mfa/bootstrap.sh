#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MAMBA_ROOT_PREFIX="$ROOT/.mamba"
export MAMBA_ROOT_PREFIX

if [[ ! -x "$ROOT/bin/micromamba" ]]; then
  echo "Missing $ROOT/bin/micromamba" >&2
  echo "Download micromamba for linux-64 before running this script." >&2
  exit 1
fi

"$ROOT/bin/micromamba" create -y -p "$ROOT/.env" -c conda-forge \
  montreal-forced-aligner 'kaldi=*=cpu*'

export MFA_ROOT_DIR="$ROOT/work"
export PATH="$ROOT/.env/bin:$PATH"
"$ROOT/.env/bin/mfa" model download acoustic greek_cv
"$ROOT/.env/bin/mfa" model download dictionary greek_cv
"$ROOT/.env/bin/mfa" train_g2p greek_cv "$ROOT/models/greek_cv_g2p.zip" \
  --temporary_directory "$ROOT/work/g2p-fresh" \
  --no_use_mp \
  --overwrite
