# Daniel Brooks — complete call pitch

This is the current English pitch for the Orpheus voice (`aura-2-orpheus-en`).
The bot uses these lines as local WAV clips. The wording below is the source
copy; keep it synchronized with `scripts/generate_voices.sh` before regenerating
audio.

## Call-center-provided core pitch

This is the source wording supplied by the call center. In the runtime,
`[Name]` is replaced with the configured bot name (`Daniel Brooks`):

> Hi, this is [Name]. I’m calling because there may be some new benefits and coverage options available for people in your age group, and I just need to verify a couple of details to see whether you qualify. It’ll only take a moment. Can I ask how old you are?
>
> **If the caller is 50–80:** Perfect, thank you. And are you currently receiving any type of coverage or benefits that would help your family with expenses at the time of death?
>
> The reason I’m asking is that there are coverage options designed to help families with those costs, and a licensed specialist can check what you may be eligible for. I can transfer you now so they can go over the details with you. Is that okay?

The final deployment retains a clear automated-call disclosure where required
by the calling program or applicable law. Testing defaults to neutral wording;
set `AUTOMATED_CALL_DISCLOSURE=1` and regenerate the identity clips before
production. The bot must not claim to be a human or a licensed specialist.

## Main call flow

### Opening — `opening_benefits.wav`

> Hi, this is Daniel Brooks with American Resource Center. I’m reaching out because there may be a benefit available for people in your age group, and I just need to confirm one quick thing before I explain why I’m calling. Are you between 50 and 80?

### If the caller gives an age from 50 through 80

> Perfect, thank you. That’s exactly what I needed to confirm. There may be options available for you based on your age, and one of our licensed specialists can explain the details and answer your questions. Please stay with me for a moment while I connect you with the specialist.

Disposition: `RXFER` / `TRANSFER`  
Audio: `transfer_confirm.wav`

### If the caller gives an age outside 50 through 80

> Thank you for letting me know. These options are currently for people between 50 and 80. I appreciate your time. Goodbye.

Disposition: `NOT_ELIGIBLE` / `DISPOSE`  
Audio: `not_eligible.wav`

### If the caller does not answer the age question clearly

> I just need your age to check the available options. How old are you?

Audio: `age_reask.wav`. After repeated non-progress, the call becomes
`NI` / `DISPOSE`.

### Coverage answer: yes, Medicare, insurance, policy, plan, or similar

> Thank you, I understand.

Audio: `coverage_yes.wav` (the flow then continues to the transfer offer).

### Coverage answer: no, none, or not covered

> Thank you, I understand.

Audio: `coverage_no.wav` (the flow then continues to the transfer offer).

### Transfer offer

> The reason I am asking is that there are coverage options designed to help families with those costs, and a licensed specialist can check what you may be eligible for. I can transfer you now so they can go over the details with you. Is that okay?

Audio: `transfer_offer.wav`

### Caller agrees to the transfer

> Great, thank you. Please hold while I connect you with a licensed specialist.

Disposition: `RXFER` / `TRANSFER`
Audio: `transfer_confirm.wav`

## Questions the caller may ask

### Identity or company

> I’m Daniel Brooks, an automated calling assistant with Your Company. I can help with the initial questions, and a licensed specialist can provide the full details.

Audio: `identity.wav`

### Last name

> My name is Daniel Brooks. I’m an automated calling assistant, not a licensed agent. A licensed specialist can identify themselves when I connect you.

Audio: `last_name.wav`

### “Are you a bot?” or “Are you human?”

> I’m Daniel Brooks, an automated calling assistant helping with the initial questions. A licensed specialist can provide the full details, and I’ll keep this brief.

Audio: `bot_identity.wav`

### What is this about?

> It’s about benefits and coverage options that may be available for people in your age group. I only need to verify a couple of details.

Audio: `what_this_is.wav`

### Where are you calling from?

> I’m calling on behalf of Your Company from its contact center. A licensed specialist can provide the official company details.

Audio: `location.wav`

### How long will it take?

> It should only take a moment. I’ll ask a couple of questions, and you can stop at any time.

Audio: `time_required.wav`

### Why do you need this information?

> I only need a couple of basic answers to see whether a licensed specialist should review the available options with you.

Audio: `information_needed.wav`

### Caller asks for a repeat

> Of course. I’ll say it again slowly. I’m checking whether you may qualify for benefits and coverage options, and a licensed specialist can explain the details.

Audio: `repeat_explain.wav`

### Privacy or payment question

> I do not need your Social Security number, bank details, or payment information. A licensed specialist can explain the official details.

Audio: `privacy.wav`

### Who is the licensed specialist?

> A licensed specialist can answer detailed questions and explain any available options.

Audio: `licensed_specialist.wav`

### Is this required?

> There is no obligation to continue. You can stop the call or request a callback at any time.

Audio: `no_pressure.wav`

### Thank you

> You’re welcome.

Audio: `youre_welcome.wav`

## Disposition scripts

| Caller situation | Spoken response | Status/action | Audio |
|---|---|---|---|
| Direct DNC request | “Understood. We will not contact you again. Goodbye.” | `DNC` / `DNC` | `dnc.wav` |
| Profanity or abusive insult | “Understood. We will not contact you again. Goodbye.” | `DNC` / `DNC` | `dnc.wav` |
| Direct Not Interested | “No problem. Thank you for your time. Goodbye.” | `NI` / `DISPOSE` | `not_interested.wav` |
| Not Interested + don’t call again | “No problem. Thank you for your time. We will mark this as not interested. Goodbye.” | `NI` / `DISPOSE` | `not_interested.wav` |
| Business/non-residential number | “Thanks for letting me know. We will mark this as a business or non-residential number. Goodbye.” | `BUSINESS_NUMBER` / `DISPOSE` | `dnc.wav` |
| Wrong number | “I am sorry about that. We will update our records. Goodbye.” | `WRONG_NUMBER` / `DISPOSE` | `wrong_number.wav` |
| Busy or cannot talk | “No problem. I understand you are busy. Goodbye.” | `BUSY` / `DISPOSE` | `busy_ack.wav` |
| Call me later | “Of course. We will arrange a callback. Thank you.” | `CALLBK` / `CALLBACK` | `callback.wav` |
| No response after opening | “I’m sorry, I’m not hearing a response. We’ll mark this as no answer. Goodbye.” | `NO_ANSWER` / `DISPOSE` | `no_answer.wav` |
| Caller asks for a human | “Certainly. Please hold while I connect you.” | `RXFER` / `TRANSFER` | `transfer.wav` |
| Caller accepts specialist transfer | “Great, thank you. Please hold while I connect you with a licensed specialist.” | `RXFER` / `TRANSFER` | `transfer_confirm.wav` |
| Repeated unrelated/non-progress turns | “No problem. Thank you for your time. Goodbye.” | `NI` / `DISPOSE` | `not_interested.wav` |
| Call reaches 90-second limit | “Thank you. We have reached the call time limit. Goodbye.” | `CALL_LIMIT` / `DISPOSE` | `not_interested.wav` |

Business/non-residential examples include a business number, office, workplace,
school, salon, clinic, store, shop, restaurant, hotel, or “not a home number.”

## Small reusable acknowledgements

These clips are available for future flow refinements:

- `acknowledge.wav` — “I understand. Thank you for explaining that.”
- `one_moment.wav` — “One moment, please.”
- `go_ahead.wav` — “Of course. Take your time.”
- `yes_ack.wav` — “Okay, thank you.”
- `no_ack.wav` — “That is completely fine.”
- `hold.wav` — “Please hold for just a moment while I connect you.”
- `goodbye.wav` — “Thank you for your time. Goodbye.”
- `callback_offer.wav` — “If now is not convenient, I can arrange a callback for another time.”
- `busy_ack.wav` — “No problem. I understand you are busy.”
- `unclear_question.wav` — “I want to make sure I understood you. Could you say that one more time?”
- `unclear.wav` — “I am sorry, I can only help with this offer, a callback, or connecting you to a team member. Which would you prefer?”

## Matching audio to the code

The Go classifier is the source of truth for routing and dispositions. The
generator is the source of truth for WAV text. After changing any sentence,
regenerate only the affected file, for example:

```bash
VOICE_MODELS=aura-2-orpheus-en \
VOICE_FILES=opening_benefits.wav \
FORCE_REGENERATE=1 \
./scripts/generate_voices.sh
```
