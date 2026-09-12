#!/usr/bin/env bash
set -euo pipefail
APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$APP_DIR"
command -v python3 >/dev/null || { echo "python3 is required"; exit 1; }
command -v curl >/dev/null || { echo "curl is required"; exit 1; }
command -v unzip >/dev/null || { echo "unzip is required"; exit 1; }
python3 -m venv .venv
.venv/bin/python -m pip install --upgrade pip vosk
mkdir -p models
MODEL_DIR="models/vosk-model-small-en-us-0.15"
if [[ ! -d "$MODEL_DIR" ]]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -L --fail --retry 3 -o "$tmp/model.zip" https://alphacephei.com/vosk/models/vosk-model-small-en-us-0.15.zip
  unzip -q "$tmp/model.zip" -d models
fi
echo "Vosk is ready: $MODEL_DIR"
