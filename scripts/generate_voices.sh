#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${VOICES_DIR:-$ROOT_DIR/voices}"
: "${DEEPGRAM_API_KEY:?Set DEEPGRAM_API_KEY in the environment; never commit it}"

# Keep the secret out of the curl process arguments. The temporary config is
# private, removed on exit, and never written into the repository.
AUTH_CONFIG="$(mktemp "${TMPDIR:-/tmp}/deepgram-tts.XXXXXX")"
chmod 600 "$AUTH_CONFIG"
printf 'header = "Authorization: Token %s"\n' "$DEEPGRAM_API_KEY" > "$AUTH_CONFIG"
cleanup() { rm -f "$AUTH_CONFIG"; }
trap cleanup EXIT

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required to write voice manifests" >&2
  exit 1
fi

# This project is intentionally pinned to one masculine Aura-2 voice while
# the persona is being tested. Add other models only after explicitly opting in
# and updating the persona/audio set.
MODEL="aura-2-orpheus-en"
model_spec="${VOICE_MODELS:-$MODEL}"
if [[ "$model_spec" != "$MODEL" ]]; then
  echo "This batch is pinned to $MODEL; remove other voice models before running it." >&2
  exit 1
fi
MODELS=("$MODEL")

FIRST_NAME="${BOT_FIRST_NAME:-Daniel}"
LAST_NAME="${BOT_LAST_NAME:-Brooks}"
BOT_FULL_NAME="${BOT_FULL_NAME:-$FIRST_NAME $LAST_NAME}"
BOT_COMPANY="${BOT_COMPANY:-American Resource Center}"
DISCLOSURE="${AUTOMATED_CALL_DISCLOSURE:-0}"
if [[ "$DISCLOSURE" == "1" || "$DISCLOSURE" == "true" || "$DISCLOSURE" == "yes" ]]; then
  IDENTITY_TEXT="I am $BOT_FULL_NAME, an automated calling assistant with $BOT_COMPANY. I can help with the initial questions, and a licensed specialist can provide the full details."
  LAST_NAME_TEXT="My name is $BOT_FULL_NAME. I am an automated calling assistant, not a licensed agent. A licensed specialist can identify themselves when I connect you."
  BOT_IDENTITY_TEXT="I am $BOT_FULL_NAME, an automated calling assistant helping with the initial questions. A licensed specialist can provide the full details, and I will keep this brief."
else
  IDENTITY_TEXT="I am $BOT_FULL_NAME with $BOT_COMPANY. I can help with the initial questions, and a licensed specialist can provide the full details."
  LAST_NAME_TEXT="My name is $BOT_FULL_NAME. I can help with the initial questions, and a licensed specialist can identify themselves when I connect you."
  BOT_IDENTITY_TEXT="I am $BOT_FULL_NAME with Your Company. I handle the initial questions, and a licensed specialist can provide the full details."
fi

force_regenerate="${FORCE_REGENERATE:-0}"
requested_files="${VOICE_FILES:-}"
max_parallel="${MAX_PARALLEL:-4}"
if ! [[ "$max_parallel" =~ ^[1-9][0-9]*$ ]]; then
  echo "MAX_PARALLEL must be a positive integer" >&2
  exit 1
fi

FILES=(
  opening.wav opening_benefits.wav age_reask.wav age_confirm.wav not_eligible.wav
  coverage_question.wav coverage_yes.wav coverage_no.wav transfer_offer.wav
  transfer_confirm.wav identity.wav last_name.wav bot_identity.wav what_this_is.wav
  time_required.wav information_needed.wav repeat_explain.wav youre_welcome.wav
  acknowledge.wav one_moment.wav go_ahead.wav privacy.wav no_pressure.wav
  licensed_specialist.wav location.wav callback_offer.wav busy_ack.wav
  unclear_question.wav yes_ack.wav no_ack.wav hold.wav goodbye.wav why_calling.wav
  dnc.wav wrong_number.wav callback.wav transfer.wav not_interested.wav unclear.wav
  no_answer.wav acknowledge_natural.wav age_handoff.wav thinking.wav warm_ack.wav
)

should_generate_file() {
  if [[ -z "$requested_files" ]]; then
    return 0
  fi
  local wanted
  IFS=',' read -r -a requested <<< "$requested_files"
  for wanted in "${requested[@]}"; do
    if [[ "$wanted" == "$1" ]]; then
      return 0
    fi
  done
  return 1
}

generate() {
  local out_dir="$1" file="$2" text="$3"
  if ! should_generate_file "$file"; then
    return
  fi
  if [[ -s "$out_dir/$file" && "$force_regenerate" != "1" ]]; then
    return
  fi
  curl --fail-with-body --silent --show-error --request POST \
    --config "$AUTH_CONFIG" \
    --url "https://api.deepgram.com/v1/speak?model=${CURRENT_MODEL}&encoding=linear16&container=wav&sample_rate=8000" \
    --header "Content-Type: application/json" \
    --data "$(jq -cn --arg text "$text" '{text:$text}')" \
    --output "$out_dir/$file"
}

generate_voice() {
  local model="$1"
  local out_dir="$OUT_DIR/$model"
  CURRENT_MODEL="$model"
  mkdir -p "$out_dir"
  echo "[$model] generating ${#FILES[@]} clips"

  if [[ "$DISCLOSURE" == "1" || "$DISCLOSURE" == "true" || "$DISCLOSURE" == "yes" ]]; then
    generate "$out_dir" opening.wav "Hello, this is $BOT_FULL_NAME, an automated calling assistant with $BOT_COMPANY. Is now a good time for a brief question?"
  else
    generate "$out_dir" opening.wav "Hello, this is $BOT_FULL_NAME with $BOT_COMPANY. Is now a good time for a brief question?"
  fi
  generate "$out_dir" opening_benefits.wav "Hi, this is $BOT_FULL_NAME with $BOT_COMPANY. I am reaching out because there may be a benefit available for people in your age group, and I just need to confirm one quick thing before I explain why I am calling. Are you between 50 and 80?"
  generate "$out_dir" age_reask.wav "I just need your age to check the available options. How old are you?"
  generate "$out_dir" age_confirm.wav "Perfect, thank you. That is exactly what I needed to confirm."
  generate "$out_dir" not_eligible.wav "Thank you for letting me know. These options are currently for people between 50 and 80. I appreciate your time. Goodbye."
  generate "$out_dir" coverage_question.wav "Perfect, thank you. And are you currently receiving any type of coverage or benefits that would help your family with expenses at the time of death?"
  generate "$out_dir" coverage_yes.wav "Thank you, I understand."
  generate "$out_dir" coverage_no.wav "Thank you, I understand."
  generate "$out_dir" transfer_offer.wav "Perfect, thank you. That is exactly what I needed to confirm. There may be some options available for you based on your age, and the person who can actually go over those details with you is one of our licensed specialists. I will have them take it from here so you can get the information directly from someone who can answer your questions. Just stay with me for a moment while I get you over to the specialist, and they will pick it up from here."
  generate "$out_dir" transfer_confirm.wav "Perfect, thank you. That is exactly what I needed to confirm. There may be some options available for you based on your age, and the person who can actually go over those details with you is one of our licensed specialists. I will have them take it from here so you can get the information directly from someone who can answer your questions. Just stay with me for a moment while I get you over to the specialist, and they will pick it up from here."
  generate "$out_dir" identity.wav "$IDENTITY_TEXT"
  generate "$out_dir" last_name.wav "$LAST_NAME_TEXT"
  generate "$out_dir" bot_identity.wav "$BOT_IDENTITY_TEXT"
  generate "$out_dir" what_this_is.wav "It is about benefits and coverage options that may be available for people in your age group. I only need to verify a couple of details."
  generate "$out_dir" time_required.wav "It should only take a moment. I will ask a couple of questions, and you can stop at any time."
  generate "$out_dir" information_needed.wav "I only need a couple of basic answers to see whether a licensed specialist should review the available options with you."
  generate "$out_dir" repeat_explain.wav "Of course. I will say it again slowly. I am checking whether you may qualify for benefits and coverage options, and a licensed specialist can explain the details."
  generate "$out_dir" youre_welcome.wav "You are welcome."
  generate "$out_dir" acknowledge.wav "I understand. Thank you for explaining that."
  generate "$out_dir" acknowledge_natural.wav "Hmm... okay."
  generate "$out_dir" age_handoff.wav "There may be some options available for you based on your age, and the person who can actually go over those details with you is one of our licensed specialists. I will have them take it from here so you can get the information directly from someone who can answer your questions. Just stay with me for a moment while I get you over to the specialist, and they will pick it up from here."
  generate "$out_dir" thinking.wav "Hmm... let me make sure I have that right."
  generate "$out_dir" warm_ack.wav "All right... thank you."
  generate "$out_dir" one_moment.wav "One moment, please."
  generate "$out_dir" go_ahead.wav "Of course. Take your time."
  generate "$out_dir" privacy.wav "I do not need your Social Security number, bank details, or payment information. A licensed specialist can explain the official details."
  generate "$out_dir" no_pressure.wav "There is no obligation to continue. You can stop the call or request a callback at any time."
  generate "$out_dir" licensed_specialist.wav "A licensed specialist can answer detailed questions and explain any available options."
  generate "$out_dir" location.wav "I am calling on behalf of $BOT_COMPANY from its contact center. A licensed specialist can provide the official company details."
  generate "$out_dir" callback_offer.wav "If now is not convenient, I can arrange a callback for another time."
  generate "$out_dir" busy_ack.wav "No problem. I understand you are busy."
  generate "$out_dir" unclear_question.wav "I want to make sure I understood you. Could you say that one more time?"
  generate "$out_dir" yes_ack.wav "Okay, thank you."
  generate "$out_dir" no_ack.wav "That is completely fine."
  generate "$out_dir" hold.wav "Please hold for just a moment while I connect you."
  generate "$out_dir" goodbye.wav "Thank you for your time. Goodbye."
  generate "$out_dir" why_calling.wav "I am calling with a brief offer. Would you like to continue?"
  generate "$out_dir" dnc.wav "Understood. We will not contact you again. Goodbye."
  generate "$out_dir" wrong_number.wav "I am sorry about that. We will update our records. Goodbye."
  generate "$out_dir" callback.wav "Of course. We will arrange a callback. Thank you."
  generate "$out_dir" transfer.wav "Certainly. Please hold while I connect you."
  generate "$out_dir" not_interested.wav "No problem. Thank you for your time. Goodbye."
  generate "$out_dir" unclear.wav "I am sorry, I can only help with this offer, a callback, or connecting you to a team member. Which would you prefer?"
  generate "$out_dir" no_answer.wav "I am sorry, I am not hearing a response. We will mark this as no answer. Goodbye."

  printf '%s\n' "${FILES[@]}" | jq -R . | jq -s \
    --arg character "$BOT_FULL_NAME" \
    --arg model "$model" \
    --argjson automated_disclosure "$([[ "$DISCLOSURE" == "1" || "$DISCLOSURE" == "true" || "$DISCLOSURE" == "yes" ]] && echo true || echo false)" \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{character:$character,model:$model,automated_disclosure:$automated_disclosure,generated_at:$generated_at,files:.}' \
    > "$out_dir/manifest.json"
  echo "[$model] ready: $out_dir"
}

mkdir -p "$OUT_DIR"

# Run independent voice folders concurrently, while each voice's clips remain
# sequential and resumable. Set MAX_PARALLEL=1 for conservative rate limits.
pids=()
for model in "${MODELS[@]}"; do
  if ! [[ "$model" =~ ^aura-2-[a-z0-9-]+-en$ ]]; then
    echo "invalid Aura-2 English model: $model" >&2
    exit 1
  fi
  (generate_voice "$model") >"$OUT_DIR/.${model}.log" 2>&1 &
  pids+=("$!")
  if (( ${#pids[@]} >= max_parallel )); then
    for pid in "${pids[@]}"; do wait "$pid"; done
    pids=()
  fi
done
if (( ${#pids[@]} > 0 )); then
  for pid in "${pids[@]}"; do wait "$pid"; done
fi

printf '%s\n' "${MODELS[@]}" | jq -R . | jq -s \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{model_family:"aura-2",language:"en",generated_at:$generated_at,voices:.}' \
  > "$OUT_DIR/catalog.json"
echo "Voice catalog ready in $OUT_DIR"
