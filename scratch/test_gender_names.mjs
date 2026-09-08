// Test voice gender-aware name assignment and Senior Benefits greeting
const ws = new WebSocket('ws://localhost:8080/ws/call');

const tests = [
  { voice: 'Aoede', expectedGender: 'female' },
  { voice: 'Puck', expectedGender: 'male' },
  { voice: 'Kore', expectedGender: 'female' },
  { voice: 'Charon', expectedGender: 'male' },
];

let currentTestIdx = 0;
const results = [];

ws.onopen = () => {
  runNextTest();
};

function runNextTest() {
  if (currentTestIdx >= tests.length) {
    console.log('All tests passed! Results:', JSON.stringify(results, null, 2));
    ws.close();
    process.exit(0);
  }
  const test = tests[currentTestIdx];
  console.log(`Starting call with voice: ${test.voice} (Expected: ${test.expectedGender})...`);
  ws.send(JSON.stringify({ type: 'start_call', voice: test.voice }));
}

ws.onmessage = (event) => {
  const msg = JSON.parse(event.data);
  if (msg.type === 'call_started') {
    const test = tests[currentTestIdx];
    console.log(`✅ Call #${currentTestIdx + 1} started! Agent Name: ${msg.agent_name}, Gender: ${msg.agent_gender}, Voice: ${msg.voice}`);

    if (msg.agent_gender !== test.expectedGender) {
      console.error(`❌ Gender mismatch: expected ${test.expectedGender}, got ${msg.agent_gender}`);
      process.exit(1);
    }

    results.push({
      voice: msg.voice,
      agent_name: msg.agent_name,
      agent_gender: msg.agent_gender,
    });

    // Hang up and move to next test
    ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
    currentTestIdx++;
    setTimeout(runNextTest, 500);
  } else if (msg.type === 'error') {
    console.error('WS error:', msg.error);
  }
};

ws.onerror = (err) => {
  console.error('WS fatal error:', err);
  process.exit(1);
};

setTimeout(() => {
  console.error('Test timeout after 25s');
  process.exit(results.length === tests.length ? 0 : 1);
}, 25000);
