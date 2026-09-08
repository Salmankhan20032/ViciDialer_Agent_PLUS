// Test live full-duplex session with Gemini Live
const ws = new WebSocket('ws://localhost:8080/ws/call');

let audioPacketsReceived = 0;
let transcriptReceived = false;

ws.onopen = () => {
  console.log('WS Open. Starting live call...');
  ws.send(JSON.stringify({ type: 'start_call', voice: 'Aoede' }));
};

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);
  if (msg.type === 'call_started') {
    console.log('✅ Call started! Call ID:', msg.call_id, 'Active Key:', msg.active_key);
    console.log('Key Pool Status:', JSON.stringify(msg.key_pool));
  } else if (msg.type === 'live_audio') {
    audioPacketsReceived++;
    if (audioPacketsReceived === 1) {
      console.log('🔊 First audio packet arrived from Gemini Live! Base64 length:', msg.audio_pcm24k.length);
    }
  } else if (msg.type === 'live_transcript') {
    transcriptReceived = true;
    console.log(`💬 Transcript [${msg.role}]:`, msg.text);
  } else if (msg.type === 'live_turn_complete') {
    console.log(`✅ Turn complete received. Audio packets received: ${audioPacketsReceived}`);

    // Now test sending a PCM chunk (16kHz silence)
    console.log('Testing sending PCM audio chunk...');
    const fakePCM16 = Buffer.alloc(1024); // 512 samples of silence
    ws.send(JSON.stringify({
      type: 'live_pcm_chunk',
      audio_base64: fakePCM16.toString('base64'),
    }));

    // Wait 1 second then hang up
    setTimeout(() => {
      console.log('Ending call with hangup...');
      ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
    }, 1000);
  } else if (msg.type === 'call_ended') {
    console.log('✅ Call ended successfully with disposition:', msg.disposition);
    ws.close();
    process.exit(0);
  } else if (msg.type === 'error') {
    console.error('❌ Error received:', msg.error);
  }
};

ws.onerror = (err) => {
  console.error('WS error:', err);
  process.exit(1);
};

setTimeout(() => {
  console.log('Test completed or timed out.');
  ws.close();
  process.exit(audioPacketsReceived > 0 ? 0 : 1);
}, 15000);
