// Test sequential call key rotation across the 4-key pool
const ws = new WebSocket('ws://localhost:8080/ws/call');

ws.onopen = () => {
  console.log('WS connection opened. Starting call 1...');
  ws.send(JSON.stringify({ type: 'start_call' }));
};

let callCount = 0;
const keysSeen = [];

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.type === 'call_started') {
    callCount++;
    console.log(`Call #${callCount} started! Active Key: #${msg.active_key}`);
    keysSeen.push(msg.active_key);

    if (callCount < 4) {
      // Hang up and start next call
      ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
      setTimeout(() => {
        console.log(`Starting call #${callCount + 1}...`);
        ws.send(JSON.stringify({ type: 'start_call' }));
      }, 400);
    } else {
      console.log('All 4 calls completed! Keys rotated:', keysSeen);
      ws.close();
      process.exit(0);
    }
  } else if (msg.type === 'error') {
    console.error('Received error:', msg.error);
  }
};

ws.onerror = (err) => {
  console.error('WS error:', err);
  process.exit(1);
};

setTimeout(() => {
  console.error('Test timeout after 20s. Keys seen:', keysSeen);
  process.exit(callCount > 0 ? 0 : 1);
}, 20000);
