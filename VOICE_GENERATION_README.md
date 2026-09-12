# Deepgram voice generation

This repository generates local telephony WAV clips once, then serves them
from the bot. Runtime calls do not contact Deepgram and do not need an API key.
The current project is intentionally pinned to the single masculine
`aura-2-orpheus-en` voice and the Daniel Brooks persona.

## Keep the key out of files

Do **not** put the real Deepgram key in this README, source code, Git, browser
JavaScript, shell history, or command-line arguments. The key belongs only in
the current shell environment. The safest interactive setup is:

```bash
read -rsp "Deepgram API key: " DEEPGRAM_API_KEY
printf '\n'
export DEEPGRAM_API_KEY
```

The generator copies the key into a private temporary `curl` config file with
mode `600`, uses it for the request, and removes it on exit. This avoids
exposing the key in `ps` output. Rotate a key immediately if it was pasted into
chat, a terminal command, a log, or a repository.

## Generate the complete Orpheus set

From the repository root:

```bash
VOICE_MODELS=aura-2-orpheus-en \
MAX_PARALLEL=1 \
FORCE_REGENERATE=1 \
./scripts/generate_voices.sh
```

The output is written to:

```text
voices/aura-2-orpheus-en/
```

The current set contains 40 clips, including the opening, age and coverage
questions, identity answers, transfer/callback/DNC dispositions, Busy,
Not-Interested, and No-Answer. `manifest.json` records the persona, model, and
file names. `catalog.json` records the one enabled voice.

## Regenerate one clip only

Use `VOICE_FILES` to avoid downloading the whole set. For example, to update
the opening script:

```bash
VOICE_MODELS=aura-2-orpheus-en \
VOICE_FILES=opening_benefits.wav \
MAX_PARALLEL=1 \
FORCE_REGENERATE=1 \
./scripts/generate_voices.sh
```

The persona can be changed for a future generation with environment variables
(the running Go service must be changed to match):

```bash
BOT_FIRST_NAME=Daniel BOT_LAST_NAME=Brooks \
VOICE_FILES=opening_benefits.wav \
FORCE_REGENERATE=1 \
./scripts/generate_voices.sh
```

## What the script sends

For each selected clip, the script sends a JSON body such as:

```json
{"text":"The sentence to speak"}
```

to this Deepgram endpoint:

```text
POST https://api.deepgram.com/v1/speak?model=aura-2-orpheus-en&encoding=linear16&container=wav&sample_rate=8000
```

Headers are `Content-Type: application/json` and an `Authorization: Token …`
header supplied through the temporary private `curl` config. The response is
saved as an 8 kHz linear-PCM WAV file for Asterisk/VICIdial playback.

## Verify the result

```bash
python3 - <<'PY'
import json, os
root = 'voices/aura-2-orpheus-en'
manifest = json.load(open(root + '/manifest.json'))
missing = [name for name in manifest['files']
           if not os.path.isfile(os.path.join(root, name))]
print('character:', manifest['character'])
print('model:', manifest['model'])
print('clips:', len(manifest['files']))
print('missing:', missing)
PY
```

Start the local service with `VOICE_MODEL=aura-2-orpheus-en ./vicidial-bot`.
The browser and the Go service use `/voices/aura-2-orpheus-en/<clip>.wav`; no
Deepgram request is made during a call.

## Troubleshooting

- `DEEPGRAM_API_KEY is required`: export the key in the same shell that runs the script.
- HTTP 401: the key is invalid, expired, revoked, or belongs to a different project.
- HTTP 429: rerun with `MAX_PARALLEL=1` and wait for the provider rate limit.
- A changed sentence did not regenerate: set `FORCE_REGENERATE=1` and use `VOICE_FILES=<name>.wav`.
- Never use `VOICE_MODELS=all` in this pinned project; the script rejects non-Orpheus selections.
