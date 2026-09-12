# VICIdial Bot

Client-hosted English voice bot for VICIdial/Asterisk. Daniel Brooks with
American Resource Center has a stateful
qualification flow: age check (50–80), coverage question, specialist transfer
consent, and safe dispositions. Common responses are pre-generated WAV files;
live calls do not use Gemini, GPT, or a runtime TTS API.

## Start locally

```bash
export DEEPGRAM_API_KEY='your-key'   # only for one-time voice generation
FORCE_REGENERATE=1 ./scripts/generate_voices.sh
VOICE_MODEL=aura-2-orpheus-en go run .
```

The generator is intentionally pinned to the single masculine
`aura-2-orpheus-en` voice while Daniel is being tested. It will reject other
voice-model selections so a large batch cannot be started accidentally.
The maximum call window is 90 seconds.

Repeated non-progress or unrelated answers are disposed as `NI`; a direct “I’m
busy” is disposed as `BUSY`, while “call me later” remains `CALLBK`. A confirmed
handoff to a real person is returned as `TRANSFER` + `RXFER`. Business,
workplace, school, salon, and other non-residential numbers are returned as
`BUSINESS_NUMBER`.

Set up the free local Vosk recognizer once, then start it alongside the bot:

```bash
./scripts/setup_vosk.sh
.venv/bin/python scripts/vosk_server.py &
```

Open http://localhost:8080 for the test console. **Start a Call** connects the
microphone through the local adapter; the mic button can be toggled without
ending the call. The flow understands birth years
(for example, 1960 becomes age 66 in 2026), identity questions, last-name
questions, DNC requests, callbacks, and transfer consent.

The web console includes a local voice selector (only folders with a complete
manifest are shown) and persists the most recent disposition in the **Last
Call** card. Clear profanity/abusive insults are treated as a `DNC` request;
the browser never displays the matched term in that card.

During this testing phase, the identity clips use neutral wording. For the
final compliant deployment, restore the automated-call disclosure in both text
and audio with:

```bash
AUTOMATED_CALL_DISCLOSURE=1 FORCE_REGENERATE=1 \
  VOICE_FILES=opening.wav,identity.wav,last_name.wav,bot_identity.wav \
  ./scripts/generate_voices.sh
```

## Production shape

Run Asterisk AMD first, then the bot service, the local Vosk server, and
`asterisk-adapter` on a private
Linux host reachable by the customer’s Asterisk server. The adapter receives
only human-classified calls. It receives real-time Asterisk AudioSocket audio,
sends it to Vosk over loopback, returns
Daniel’s pre-generated WAV audio, and posts finalized transcripts to the bot
API. Chrome is not part of the production call path.

```bash
go build -o vicidial-bot .
go build -o asterisk-adapter ./cmd/asterisk-adapter
set -a; . ./.env; set +a
./vicidial-bot &
./.venv/bin/python scripts/vosk_server.py &
./asterisk-adapter
```

See [`deploy/asterisk/README.md`](deploy/asterisk/README.md) for the
AudioSocket dialplan shape, required environment variables, and the transfer /
disposition webhook handoff. Do not put keys in Git.

## Endpoints

- `POST /api/calls/start`
- `GET /api/stats`
- `POST /api/calls/turn` with `{ "session_id", "text" }`
- `POST /api/calls/no-answer` with `{ "session_id" }`
- `POST /api/calls/hangup`
- `POST /api/vicidial/disposition`
- `POST /api/vicidial/transfer`
- `POST /api/vicidial/amd`
