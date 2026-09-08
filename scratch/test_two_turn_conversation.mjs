import fs from 'fs';

const wavBuffer = fs.readFileSync('scratch/answer.wav');
// afinfo shows audio data file offset is 4096 bytes for this WAV
const pcmBytes = wavBuffer.subarray(4096);

console.log(`Loaded test speech audio: ${pcmBytes.length} bytes PCM16 (~${(pcmBytes.length / 32000).toFixed(2)}s)`);

const ws = new WebSocket('ws://localhost:8080/ws/call');

let agentName = '';
let turnCount = 0;
let secondTurnTranscript = '';

ws.onopen = () => {
  console.log('Connected to WebSocket. Starting call with Aoede...');
  ws.send(JSON.stringify({ type: 'start_call', voice: 'Aoede' }));
};

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);

  if (msg.type === 'call_started') {
    agentName = msg.agent_name || 'Sarah';
    console.log(`✅ Call started with Agent ${agentName}!`);
  } else if (msg.type === 'live_transcript') {
    if (msg.role === 'bot') {
      if (turnCount >= 1) {
        secondTurnTranscript += msg.text;
      }
      process.stdout.write(msg.text);
    }
  } else if (msg.type === 'live_turn_complete') {
    turnCount++;
    console.log(`\n\n[Turn #${turnCount} Complete from ${agentName}]`);

    if (turnCount === 1) {
      console.log('\n🎙️ Homeowner answering: "I am sixty-five years old." (Streaming PCM16 chunks)...');

      // Stream the PCM audio in 2048-byte chunks (~64ms each) with 50ms intervals
      const chunkSize = 2048;
      let offset = 0;
      let trailingSilenceFrames = 15; // ~750ms trailing silence for Gemini VAD to detect end-of-speech

      const interval = setInterval(() => {
        if (offset >= pcmBytes.length) {
          if (trailingSilenceFrames > 0) {
            trailingSilenceFrames--;
            // Send silence frame (zero PCM)
            const silenceChunk = new Uint8Array(chunkSize);
            let binary = '';
            for (let i = 0; i < silenceChunk.length; i++) {
              binary += String.fromCharCode(silenceChunk[i]);
            }
            ws.send(JSON.stringify({
              type: 'live_pcm_chunk',
              audio_base64: btoa(binary),
            }));
            return;
          }

          clearInterval(interval);
          console.log('✅ Finished streaming homeowner voice + trailing silence. Waiting for agent reply...');
          return;
        }

        const chunk = pcmBytes.subarray(offset, Math.min(offset + chunkSize, pcmBytes.length));
        offset += chunkSize;

        let binary = '';
        for (let i = 0; i < chunk.length; i++) {
          binary += String.fromCharCode(chunk[i]);
        }
        ws.send(JSON.stringify({
          type: 'live_pcm_chunk',
          audio_base64: btoa(binary),
        }));
      }, 50);

    } else if (turnCount >= 2) {
      console.log(`\n🎉 SUCCESS! Agent ${agentName} heard the homeowner's voice and replied:`);
      console.log(`Transcript: "${secondTurnTranscript.trim()}"`);

      // Hangup
      console.log('\nHanging up call cleanly...');
      ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
    }
  } else if (msg.type === 'call_ended') {
    console.log(`\n✅ Call ended cleanly with disposition: ${msg.disposition}`);
    ws.close();
    process.exit(0);
  }
};

ws.onerror = (err) => {
  console.error('WS error:', err);
  process.exit(1);
};

setTimeout(() => {
  console.log('\nTest completed or timed out.');
  ws.close();
  process.exit(turnCount >= 2 ? 0 : 1);
}, 35000);
