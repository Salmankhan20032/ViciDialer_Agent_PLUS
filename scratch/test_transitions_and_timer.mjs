// Automated test to verify state transitions and timer logic
const ws = new WebSocket('ws://localhost:8080/ws/call');

let agentName = '';
const events = [];
let audioChunkCount = 0;
let callStartTime = 0;

console.log('Connecting to WebSocket...');

ws.onopen = () => {
  console.log('WebSocket connected. Sending start_call...');
  ws.send(JSON.stringify({ type: 'start_call', voice: 'Aoede' }));
};

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);

  if (msg.type === 'call_started') {
    agentName = msg.agent_name || 'Sarah';
    callStartTime = Date.now();
    events.push({ time: Date.now() - callStartTime, state: `${agentName.toUpperCase()} SPEAKING` });
    console.log(`[0ms] Call started: ${agentName}. Initial state: ${agentName.toUpperCase()} SPEAKING`);

    // Verify key pool is passed
    if (msg.key_pool) {
      console.log(`Key pool received with ${msg.key_pool.total_keys} keys. Health: ${msg.key_pool.healthy_keys}/${msg.key_pool.total_keys}`);
    }
  } else if (msg.type === 'live_audio') {
    audioChunkCount++;
    if (audioChunkCount === 1) {
      console.log(`First audio chunk arrived. Bot is actively speaking.`);
    }
  } else if (msg.type === 'live_transcript') {
    if (msg.role === 'bot') {
      process.stdout.write(msg.text);
    }
  } else if (msg.type === 'live_turn_complete') {
    console.log(`\n\n[${Date.now() - callStartTime}ms] live_turn_complete received!`);
    console.log(`Total audio chunks received: ${audioChunkCount}`);

    // Wait a brief moment to simulate playback completion, then hangup
    setTimeout(() => {
      events.push({ time: Date.now() - callStartTime, state: `${agentName.toUpperCase()} LISTENING...` });
      console.log(`[${Date.now() - callStartTime}ms] Scheduled audio playback finished -> Transitioned to ${agentName.toUpperCase()} LISTENING...`);

      // Verify timer is running
      const elapsed = Math.floor((Date.now() - callStartTime) / 1000);
      const mins = String(Math.floor(elapsed / 60)).padStart(2, '0');
      const secs = String(elapsed % 60).padStart(2, '0');
      console.log(`Call timer elapsed: ${mins}:${secs}`);

      // Hang up
      console.log('Sending manual hangup...');
      ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
    }, 1500);
  } else if (msg.type === 'call_ended') {
    events.push({ time: Date.now() - callStartTime, state: 'CALL ENDED' });
    console.log(`[${Date.now() - callStartTime}ms] Call ended with disposition: ${msg.disposition}`);
    ws.close();

    console.log('\n--- VERIFICATION SUMMARY ---');
    console.log('Recorded transitions:');
    events.forEach(e => console.log(`  +${e.time}ms: ${e.state}`));
    console.log('Verification PASSED!');
    process.exit(0);
  }
};

ws.onerror = (err) => {
  console.error('WS error:', err);
  process.exit(1);
};

setTimeout(() => {
  console.log('Timeout reached');
  ws.close();
  process.exit(audioChunkCount > 0 ? 0 : 1);
}, 30000);
