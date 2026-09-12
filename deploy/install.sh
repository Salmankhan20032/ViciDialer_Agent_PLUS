#!/usr/bin/env bash
set -euo pipefail
APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v go >/dev/null || { echo "Go 1.23+ is required"; exit 1; }
mkdir -p "$APP_DIR/voices"
if [[ -n "${DEEPGRAM_API_KEY:-}" ]]; then
  "$APP_DIR/scripts/generate_voices.sh"
else
  echo "DEEPGRAM_API_KEY is not set; skipping one-time voice generation."
fi
"$APP_DIR/scripts/setup_vosk.sh"
go build -o "$APP_DIR/vicidial-bot" "$APP_DIR"
go build -o "$APP_DIR/asterisk-adapter" "$APP_DIR/cmd/asterisk-adapter"
echo "Built $APP_DIR/vicidial-bot"
echo "Built $APP_DIR/asterisk-adapter"
echo "Run: LISTEN_ADDR=:8080 $APP_DIR/vicidial-bot"
echo "Run: $APP_DIR/.venv/bin/python $APP_DIR/scripts/vosk_server.py &"
echo "Run: AUDIO_SOCKET_ADDR=:9019 BOT_URL=http://127.0.0.1:8080 $APP_DIR/asterisk-adapter"
