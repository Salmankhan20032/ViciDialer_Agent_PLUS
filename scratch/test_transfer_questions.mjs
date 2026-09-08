import fs from 'fs';

function loadPCM(filename) {
  const buf = fs.readFileSync(filename);
  return buf.subarray(4096);
}

const pcmAge = loadPCM('scratch/answer.wav'); // "I am sixty-five years old."
const pcmCoverage = loadPCM('scratch/t2.wav'); // "No, I do not have any coverage right now."
const pcmAgreeXfer = loadPCM('scratch/t3.wav'); // "Yes, that sounds good, you can transfer me."
const pcmQuestion = loadPCM('scratch/t4.wav'); // "Is this free to speak with the specialist?"

console.log('Test audio loaded for 4 conversational turns.');

const ws = new WebSocket('ws://localhost:8080/ws/call');

let agentName = '';
let botTurn = 0;
let finalDisposition = '';

function streamAudio(pcmBytes, onDone) {
  const chunkSize = 2048;
  let offset = 0;
  let trailingSilence = 16; // ~800ms trailing silence for turn completion

  const interval = setInterval(() => {
    if (offset >= pcmBytes.length) {
      if (trailingSilence > 0) {
        trailingSilence--;
        const silence = new Uint8Array(chunkSize);
        let bin = '';
        for (let i = 0; i < silence.length; i++) bin += String.fromCharCode(silence[i]);
        ws.send(JSON.stringify({ type: 'live_pcm_chunk', audio_base64: btoa(bin) }));
        return;
      }
      clearInterval(interval);
      if (onDone) onDone();
      return;
    }

    const chunk = pcmBytes.subarray(offset, Math.min(offset + chunkSize, pcmBytes.length));
    offset += chunkSize;
    let bin = '';
    for (let i = 0; i < chunk.length; i++) bin += String.fromCharCode(chunk[i]);
    ws.send(JSON.stringify({ type: 'live_pcm_chunk', audio_base64: btoa(bin) }));
  }, 45);
}

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
    process.stdout.write(`\n💬 [${msg.role === 'bot' ? 'OPPONENT (Bot)' : 'MY DIALOG (User)'}]: "${msg.text}"`);
  } else if (msg.type === 'disposition_update') {
    finalDisposition = msg.status;
    console.log(`\n🎯 VICIdial Disposition updated: ${msg.status} (${msg.notes})`);
  } else if (msg.type === 'live_turn_complete') {
    botTurn++;
    console.log(`\n[Agent ${agentName} Turn #${botTurn} Complete]`);

    if (botTurn === 1) {
      console.log('🎙️ User replying: "I am sixty-five years old."');
      streamAudio(pcmAge, () => {
        console.log('-> Waiting for Turn #2 (Coverage check)...');
      });
    } else if (botTurn === 2) {
      console.log('🎙️ User replying: "No, I do not have any coverage right now."');
      streamAudio(pcmCoverage, () => {
        console.log('-> Waiting for Turn #3 (Transfer offer)...');
      });
    } else if (botTurn === 3) {
      console.log('🎙️ User replying: "Yes, that sounds good, you can transfer me."');
      streamAudio(pcmAgreeXfer, () => {
        console.log('-> Waiting for Turn #4 (Questions check from agent)...');
      });
    } else if (botTurn === 4) {
      console.log('🎙️ User asking: "Is this free to speak with the specialist?"');
      streamAudio(pcmQuestion, () => {
        console.log('-> Waiting for Turn #5 (Answer question & transfer conclusion)...');
      });
    } else if (botTurn >= 5) {
      console.log(`\n🎉 Agent ${agentName} completed the full transfer and questions flow!`);
      console.log(`Final Disposition: ${finalDisposition || 'Pending'}`);
      setTimeout(() => {
        ws.send(JSON.stringify({ type: 'manual_hangup', disposition: finalDisposition || 'XFER' }));
      }, 1000);
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
  process.exit(botTurn >= 4 ? 0 : 1);
}, 60000);
