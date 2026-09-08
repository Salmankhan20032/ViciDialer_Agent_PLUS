// Test Senior Benefits greeting and turn-taking
const ws = new WebSocket('ws://localhost:8080/ws/call');

let firstTurnTranscript = '';
let agentName = '';

ws.onopen = () => {
  console.log('WS Open. Starting call with Aoede...');
  ws.send(JSON.stringify({ type: 'start_call', voice: 'Aoede' }));
};

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);
  if (msg.type === 'call_started') {
    agentName = msg.agent_name;
    console.log(`✅ Call started! Agent: ${agentName} (${msg.agent_gender})`);
  } else if (msg.type === 'live_transcript') {
    if (msg.role === 'bot' || msg.role === 'model') {
      firstTurnTranscript += msg.text;
      process.stdout.write(msg.text);
    }
  } else if (msg.type === 'live_turn_complete') {
    console.log(`\n\n✅ First turn complete from ${agentName}!`);
    console.log('Full Greeting Transcript:', firstTurnTranscript);

    // Hang up
    ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
  } else if (msg.type === 'call_ended') {
    console.log('✅ Call ended cleanly with disposition:', msg.disposition);
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
  process.exit(firstTurnTranscript.length > 0 ? 0 : 1);
}, 15000);
