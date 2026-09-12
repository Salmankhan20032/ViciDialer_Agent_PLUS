# Asterisk/VICIdial production adapter

`cmd/asterisk-adapter` is the production speech path. It accepts an Asterisk
AudioSocket connection, forwards the caller’s live PCM audio to a local Vosk
streaming recognizer, submits final utterances to the bot service, and writes
the returned local WAV back to the call. Its optional loopback WebSocket also
powers the browser’s local Vosk microphone preview; that preview is not part of
the phone call path.

## Services

Run both binaries on a private Linux host that Asterisk can reach. They must
share the same `voices/` directory.

```bash
go build -o vicidial-bot .
go build -o asterisk-adapter ./cmd/asterisk-adapter

./scripts/setup_vosk.sh
.venv/bin/python scripts/vosk_server.py &

set -a; . ./.env; set +a
LISTEN_ADDR=:8080 ./vicidial-bot
AUDIO_SOCKET_ADDR=:9019 BOT_URL=http://127.0.0.1:8080 ./asterisk-adapter
```

`DEEPGRAM_API_KEY` is only needed by `scripts/generate_voices.sh` when creating
the prerecorded TTS clips. Runtime STT is local Vosk and does not need a key.
Set `VOSK_ADDR` if the recognizer runs on another private host.

## Answering-machine detection and call path

Run AMD before opening AudioSocket. This prevents a voicemail greeting from
starting a Vosk session or consuming a bot prompt. `AMD()` sets `AMDSTATUS` to
`MACHINE`, `HUMAN`, `NOTSURE`, or `HANGUP`, and `AMDCAUSE` records why it made
the decision. Keep `NOTSURE` on the human-safe fallback until carrier-specific
testing proves a more aggressive policy is safe.

```asterisk
; Illustrative custom context — do not replace a customer's VICIdial dialplan.
same => n,Answer()
same => n,AMD(2500,1500,800,5000,100,50,3,256,1500)
same => n,NoOp(AMD status=${AMDSTATUS} cause=${AMDCAUSE})
same => n,GotoIf($["${AMDSTATUS}"="MACHINE"]?amd_machine)
same => n,GotoIf($["${AMDSTATUS}"="HANGUP"]?amd_hangup)
same => n,GotoIf($["${AMDSTATUS}"="NOTSURE"]?amd_fallback)
same => n(human),Set(BOT_AUDIO_UUID=${UUID()})
same => n,AudioSocket(${BOT_AUDIO_UUID},bot-host.internal:9019)
same => n,Hangup()
same => n(amd_fallback),Goto(human)
same => n(amd_machine),Set(VICIDIAL_STATUS=AMD)
same => n,UserEvent(BotAMD,CallID:${UNIQUEID},Status:MACHINE,Cause:${AMDCAUSE})
same => n,Hangup()
same => n(amd_hangup),Set(VICIDIAL_STATUS=NO_ANSWER)
same => n,Hangup()
```

AudioSocket supplies 16-bit, mono, 8 kHz PCM. The adapter upsamples those frames
to 16 kHz for the small English Vosk model. Vosk stays loaded locally and emits
streaming endpoint results; a lightweight RMS VAD stops a currently playing
prompt as soon as the caller starts speaking.

The adapter plays the 8 kHz local clip selected by the bot as raw AudioSocket
PCM. This avoids a runtime TTS request and its latency.

## Transfer and disposition handoff

Queue names and VICIdial status mappings are customer-specific, so this
repository does not hard-code them. Configure the customer’s internal handler
URLs to receive the final actions:

```bash
export VICIDIAL_TRANSFER_WEBHOOK_URL=http://vicidial-admin.internal/bot/transfer
export VICIDIAL_DISPOSITION_WEBHOOK_URL=http://vicidial-admin.internal/bot/disposition
```

The adapter POSTs JSON like this:

```json
{"call_id":"...","session_id":"...","disposition":"RXFER","action":"TRANSFER"}
```

The internal handler must bridge the exact active channel to the customer’s
licensed-specialist queue for `RXFER`, or write the customer-approved VICIdial
status for terminal dispositions such as `DNC`, `NI`, `BUSY`, and `CALLBK`.
This separation prevents the bot code from guessing a private queue name,
campaign, or database schema.

## Required firewall and rollout checks

- Permit only the Asterisk host to reach the adapter’s TCP port `9019`.
- Permit the adapter to reach the local Vosk listener and bot API; no runtime
  speech-to-text cloud egress is required.
- Confirm `app_amd.so` and `app_audiosocket.so` are loaded on Asterisk.
- Start with a test campaign and verify age, DNC, callback, transfer, barge-in,
  answering-machine disposal, and hangup behavior on live carrier audio.
- Measure end-of-turn to first-prompt latency at the adapter; target under one
  second before scaling concurrent calls.
