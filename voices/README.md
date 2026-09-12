# Daniel Brooks voice library

This project is currently pinned to the masculine `aura-2-orpheus-en` folder.
It contains one consistent Daniel Brooks clip set and a `manifest.json`.

The WAV files are generated once during setup and then served locally. Live
calls never need a TTS API. Run `scripts/generate_voices.sh` with
`DEEPGRAM_API_KEY` set to create the files. Set `FORCE_REGENERATE=1` when the
persona script changes. All clips use one selected voice and are encoded as
8 kHz mono PCM WAV for telephony playback.
Do not put the API key in Git or in the browser; it is only needed for
one-time generation.
