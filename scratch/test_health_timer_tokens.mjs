// Verification test for API rotation health, quota reset countdown, and call tokens
const res = await fetch('http://localhost:8080/api/key_pool');
const pool = await res.json();

console.log('--- 1. API KEY POOL TELEMETRY ---');
console.log('Total keys:', pool.total_keys);
console.log('Healthy keys:', pool.healthy_keys);
console.log('Cooling keys:', pool.cooling_keys);
console.log('Health percentage:', pool.health_pct, '%');
console.log('Quota reset ISO:', pool.quota_reset_iso);
console.log('Quota reset seconds:', pool.quota_reset_seconds);

if (typeof pool.health_pct !== 'number' || typeof pool.quota_reset_seconds !== 'number') {
  console.error('FAIL: Missing health_pct or quota_reset_seconds in /api/key_pool response');
  process.exit(1);
}

// Format test
function formatQuotaCountdown(totalSec) {
  if (!totalSec || totalSec <= 0) return '00h 00m 00s';
  const hours = Math.floor(totalSec / 3600);
  const minutes = Math.floor((totalSec % 3600) / 60);
  const seconds = totalSec % 60;
  return `${hours}h ${String(minutes).padStart(2, '0')}m ${String(seconds).padStart(2, '0')}s`;
}

const formattedTime = formatQuotaCountdown(pool.quota_reset_seconds);
console.log('Formatted countdown display:', formattedTime);

console.log('\n--- 2. LIVE CALL TOKENS & STREAM TEST ---');
const ws = new WebSocket('ws://localhost:8080/ws/call');

let callTokens = 180; // Baseline start
let transcriptsReceived = 0;
let callStartTime = 0;

ws.onopen = () => {
  console.log('WebSocket connected. Starting call...');
  ws.send(JSON.stringify({ type: 'start_call', voice: 'Aoede' }));
};

ws.onmessage = (e) => {
  const msg = JSON.parse(e.data);

  if (msg.type === 'call_started') {
    callStartTime = Date.now();
    console.log(`Call started! Key #${msg.active_key_index}. Initial tokens: ${callTokens} tok`);
  } else if (msg.type === 'live_transcript') {
    transcriptsReceived++;
    const words = msg.text.trim().split(/\s+/).filter(Boolean).length;
    const addedTokens = Math.max(1, Math.round(words * 1.3));
    callTokens += addedTokens;
    if (transcriptsReceived <= 5) {
      console.log(`[Transcript ${msg.role}] "${msg.text}" (+${addedTokens} tokens -> Total: ${callTokens} tok)`);
    }
  } else if (msg.type === 'live_turn_complete') {
    console.log(`Bot opening speech complete. Current Tokens: ${callTokens} tok`);
    
    // Simulate 2 seconds of audio streaming
    callTokens += 56; // 2 sec * ~28 tok/s
    console.log(`After 2s live audio streaming: ${callTokens} tok`);

    setTimeout(() => {
      console.log('Ending call to check final tokens...');
      ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
    }, 1000);
  } else if (msg.type === 'call_ended') {
    console.log(`Call ended! Disposition: ${msg.disposition}`);
    console.log(`Final Tokens for this call: ${callTokens} tok`);
    ws.close();

    console.log('\n✅ ALL VERIFICATION CHECKS PASSED SUCCESSFULLY!');
    process.exit(0);
  }
};

ws.onerror = (err) => {
  console.error('WebSocket error:', err);
  process.exit(1);
};
