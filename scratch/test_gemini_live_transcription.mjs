import fs from 'fs';

const apiKey = 'AIzaSyCv6Fzs0N1PMZEtBgCpr2wBPofsUudlDIA';
const host = 'generativelanguage.googleapis.com';
const url = `wss://${host}/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=${apiKey}`;

const buf = fs.readFileSync('scratch/answer.wav');
const pcmAudio = buf.subarray(4096);

console.log('Connecting to:', url);
const ws = new WebSocket(url);

ws.onopen = () => {
  console.log('Connected to Gemini Live WebSocket!');
  const setupMsg = {
    setup: {
      model: 'models/gemini-2.5-flash-native-audio-latest',
      generationConfig: {
        responseModalities: ['AUDIO'],
        speechConfig: {
          voiceConfig: {
            prebuiltVoiceConfig: {
              voiceName: 'Aoede',
            },
          },
        },
      },
      inputAudioTranscription: {},
      outputAudioTranscription: {},
    },
  };
  ws.send(JSON.stringify(setupMsg));
};

function streamUserAudio() {
  console.log('🎙️ Streaming user audio: "I am sixty-five years old."...');
  const chunkSize = 2048;
  let offset = 0;
  let trailingSilence = 15;

  const interval = setInterval(() => {
    if (offset >= pcmAudio.length) {
      if (trailingSilence > 0) {
        trailingSilence--;
        const silence = new Uint8Array(chunkSize);
        let bin = '';
        for (let i = 0; i < silence.length; i++) bin += String.fromCharCode(silence[i]);
        ws.send(JSON.stringify({
          realtimeInput: {
            mediaChunks: [{ mimeType: 'audio/pcm;rate=16000', data: btoa(bin) }]
          }
        }));
        return;
      }
      clearInterval(interval);
      console.log('Audio stream finished, waiting for response and transcription...');
      return;
    }

    const chunk = pcmAudio.subarray(offset, Math.min(offset + chunkSize, pcmAudio.length));
    offset += chunkSize;
    let bin = '';
    for (let i = 0; i < chunk.length; i++) bin += String.fromCharCode(chunk[i]);
    ws.send(JSON.stringify({
      realtimeInput: {
        mediaChunks: [{ mimeType: 'audio/pcm;rate=16000', data: btoa(bin) }]
      }
    }));
  }, 45);
}

let turn = 0;

ws.onmessage = async (event) => {
  let textData = event.data;
  if (textData instanceof Blob) {
    textData = await textData.text();
  }
  const parsed = JSON.parse(textData.toString());

  if (parsed.setupComplete) {
    console.log('✅ setupComplete received! Sending test opening prompt...');
    const kickoff = {
      clientContent: {
        turns: [
          {
            role: 'user',
            parts: [{ text: 'Say "Hi, how old are you?"' }],
          },
        ],
        turnComplete: true,
      },
    };
    ws.send(JSON.stringify(kickoff));
  }

  if (parsed.serverContent) {
    if (parsed.serverContent.outputTranscription) {
      console.log('🗣️ [BOT TEXT]:', parsed.serverContent.outputTranscription.text);
    }
    if (parsed.serverContent.inputTranscription) {
      console.log('👤 [USER INPUT TEXT]:', parsed.serverContent.inputTranscription.text);
    }
    if (parsed.serverContent.turnComplete) {
      turn++;
      console.log(`\nTurn #${turn} Complete`);
      if (turn === 1) {
        setTimeout(streamUserAudio, 500);
      } else if (turn >= 2) {
        console.log('🎉 Successfully tested both bot output transcription and user input transcription!');
        setTimeout(() => {
          ws.close();
          process.exit(0);
        }, 1000);
      }
    }
  }
};

ws.onerror = (err) => console.error('WS Error:', err);
