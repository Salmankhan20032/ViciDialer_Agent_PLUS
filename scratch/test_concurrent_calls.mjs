// Test to verify concurrent multi-call capability of the backend
const numBots = 4;
console.log(`Starting ${numBots} concurrent AI bot sessions simultaneously...`);

const promises = [];

for (let i = 1; i <= numBots; i++) {
  const p = new Promise((resolve, reject) => {
    const ws = new WebSocket('ws://localhost:8080/ws/call');
    let botName = '';

    ws.onopen = () => {
      console.log(`[Bot #${i}] Connected. Starting call...`);
      ws.send(JSON.stringify({ type: 'start_call', voice: i % 2 === 0 ? 'Aoede' : 'Puck' }));
    };

    ws.onmessage = (e) => {
      const msg = JSON.parse(e.data);
      if (msg.type === 'call_started') {
        botName = msg.agent_name;
        console.log(`[Bot #${i}] ✅ LIVE! Agent: ${botName} (Key #${msg.active_key_index})`);
      } else if (msg.type === 'live_turn_complete') {
        console.log(`[Bot #${i}] 💬 Finished opening greeting! Hanging up...`);
        ws.send(JSON.stringify({ type: 'manual_hangup', disposition: 'CH' }));
      } else if (msg.type === 'call_ended') {
        console.log(`[Bot #${i}] 🏁 Call ended successfully (${msg.disposition})`);
        ws.close();
        resolve(botName);
      }
    };

    ws.onerror = (err) => {
      console.error(`[Bot #${i}] Error:`, err);
      reject(err);
    };
  });

  promises.push(p);
}

const results = await Promise.all(promises);
console.log(`\n🎉 SUCCESS! All ${results.length} bots ran CONCURRENTLY in parallel:`, results);
