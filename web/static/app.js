let sessionId = null;
let botAudio = null;
let sending = false;
let voiceReady = false;
let selectedVoice = '';
let liveMode = false;
let liveSocket = null;
let microphoneStream = null;
let audioContext = null;
let microphoneProcessor = null;
let microphoneSource = null;
let silentGain = null;
let streamingAudio = false;
let microphoneEnabled = false;
let pendingTranscript = '';
let botAudioStartedAt = 0;
let statsTimer = null;

const dispositionLabels = {
  DNC: 'Do not call', NI: 'Not interested', BUSINESS_NUMBER: 'Business number',
  BUSY: 'Busy', NO_ANSWER: 'No answer / dead air', NOT_ELIGIBLE: 'Not eligible',
  RXFER: 'Transferred to real person', CALLBK: 'Callback requested',
  WRONG_NUMBER: 'Wrong number', CxHANG: 'Customer hangup', CALL_LIMIT: 'Call limit reached'
};

const $ = id => document.getElementById(id);

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
}

function setVoiceState(mode, caption) {
  $('orb-stage').className = `orb-stage ${mode}`;
  const labels = {idle: 'Ready to call', bot: 'Daniel is speaking', customer: 'Listening to you', ended: 'Conversation ended'};
  $('state').textContent = labels[mode] || labels.idle;
  if (caption) $('caption').textContent = caption;
  $('session-state').textContent = mode === 'customer' ? 'listening' : mode;
}

function rememberDisposition(code) {
  if (!code) return;
  const record = {code, at: new Date().toISOString()};
  $('last-call-code').textContent = record.code;
  $('last-call-label').textContent = dispositionLabels[record.code] || 'Call disposed';
  $('last-call-time').textContent = new Date(record.at).toLocaleString();
  try { localStorage.setItem('vicidial-last-disposition', JSON.stringify(record)); } catch (_) {}
}

function loadLastDisposition() {
  try {
    const record = JSON.parse(localStorage.getItem('vicidial-last-disposition'));
    if (record) rememberDisposition(record.code);
  } catch (_) {}
}

function audioUrl(file) {
  return `/voices/${String(file || '').split('/').filter(Boolean).map(encodeURIComponent).join('/')}`;
}

function stopAudio() {
  if (!botAudio) return;
  botAudio.onended = null;
  botAudio.pause();
  botAudio = null;
  botAudioStartedAt = 0;
}

function setStreamingAudio(enabled) {
  streamingAudio = Boolean(enabled && liveMode && microphoneEnabled && liveSocket && liveSocket.readyState === WebSocket.OPEN);
  if (streamingAudio) setVoiceState('customer', 'Listening…');
}

function playAudio(file, files = []) {
  stopAudio();
  const queue = files.length ? files : (file ? [file] : []);
  let index = 0;
  const playNext = () => {
    if (index >= queue.length) {
      botAudio = null;
      botAudioStartedAt = 0;
      if (liveMode) setStreamingAudio(true);
      else if (sessionId) setVoiceState('customer', microphoneEnabled ? 'Listening…' : 'Microphone off');
      return;
    }
    const audio = new Audio(audioUrl(queue[index++]));
    botAudio = audio;
    botAudioStartedAt = performance.now();
    if (liveMode) setStreamingAudio(true);
    audio.onended = () => { if (botAudio === audio) playNext(); };
    audio.onerror = audio.onended;
    audio.play().catch(() => { if (botAudio === audio) playNext(); });
  };
  playNext();
}

function add(role, text, audio, audioFiles = []) {
  const empty = $('transcript').querySelector('.empty-transcript');
  if (empty) empty.remove();
  const node = document.createElement('div');
  node.className = `turn ${role}`;
  node.innerHTML = `<span class="label">${role === 'bot' ? 'DANIEL' : 'CUSTOMER'}</span>${escapeHtml(text)}`;
  $('transcript').appendChild(node);
  $('transcript').scrollTop = $('transcript').scrollHeight;
  if (role === 'bot') setVoiceState('bot', 'Daniel is speaking…');
  if (role === 'customer') setVoiceState('customer', 'Daniel is processing the response…');
  if (audio || audioFiles.length) playAudio(audio, audioFiles);
}

async function api(path, body) {
  const response = await fetch(path, {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body || {})});
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

function setActive(active) {
  $('live').disabled = active;
  $('mic-toggle').disabled = !active;
  $('hangup').disabled = !active;
  $('voice-model').disabled = active || !voiceReady;
}

function updateMicButton() {
  $('mic-label').textContent = microphoneEnabled ? 'Mic on' : 'Mic off';
  $('mic-toggle').classList.toggle('mic-off', !microphoneEnabled);
  $('mic-toggle').setAttribute('aria-label', microphoneEnabled ? 'Turn microphone off' : 'Turn microphone on');
}

function downsampleToPCM(input, inputRate, outputRate = 16000) {
  if (inputRate === outputRate) {
    const pcm = new Int16Array(input.length);
    for (let i = 0; i < input.length; i += 1) pcm[i] = Math.max(-1, Math.min(1, input[i])) * 0x7fff;
    return pcm;
  }
  const ratio = inputRate / outputRate;
  const output = new Int16Array(Math.round(input.length / ratio));
  for (let i = 0; i < output.length; i += 1) {
    const start = Math.floor(i * ratio);
    const end = Math.min(input.length, Math.floor((i + 1) * ratio));
    let total = 0;
    for (let j = start; j < Math.max(start + 1, end); j += 1) total += input[j];
    output[i] = Math.max(-1, Math.min(1, total / Math.max(1, end - start))) * 0x7fff;
  }
  return output;
}

async function prepareLiveMicrophone() {
  if (!navigator.mediaDevices?.getUserMedia) throw new Error('Microphone access is unavailable in this browser.');
  microphoneStream = await navigator.mediaDevices.getUserMedia({audio: {echoCancellation: true, noiseSuppression: true, autoGainControl: true}});
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const socketURL = `${scheme}//${location.hostname || '127.0.0.1'}:9020/ws/stt`;
  liveSocket = new WebSocket(socketURL);
  liveSocket.binaryType = 'arraybuffer';
  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('Timed out starting local Vosk streaming.')), 5000);
    liveSocket.onmessage = event => {
      let message;
      try { message = JSON.parse(event.data); } catch (_) { return; }
      if (message.type === 'ready') { clearTimeout(timer); resolve(); return; }
      if (message.type === 'error') { clearTimeout(timer); reject(new Error(message.error)); return; }
      if (message.type === 'speech_started' && botAudio) {
        stopAudio();
        setStreamingAudio(true);
        setVoiceState('customer', 'I’m listening…');
        return;
      }
      if (message.type === 'transcript' && message.text) {
        if (sending) pendingTranscript = pendingTranscript ? `${pendingTranscript} ${message.text}` : message.text;
        else processText(message.text);
      }
    };
    liveSocket.onerror = () => { clearTimeout(timer); reject(new Error('Could not connect to the local Vosk adapter.')); };
    liveSocket.onclose = () => { if (liveMode && sessionId) setVoiceState('idle', 'Local Vosk microphone stream disconnected.'); };
  });
  microphoneEnabled = true;
  updateMicButton();
  audioContext = new (window.AudioContext || window.webkitAudioContext)();
  await audioContext.resume();
  microphoneSource = audioContext.createMediaStreamSource(microphoneStream);
  microphoneProcessor = audioContext.createScriptProcessor(2048, 1, 1);
  silentGain = audioContext.createGain();
  silentGain.gain.value = 0;
  microphoneProcessor.onaudioprocess = event => {
    if (!streamingAudio || !liveSocket || liveSocket.readyState !== WebSocket.OPEN) return;
    const pcm = downsampleToPCM(event.inputBuffer.getChannelData(0), audioContext.sampleRate);
    liveSocket.send(pcm.buffer);
  };
  microphoneSource.connect(microphoneProcessor);
  microphoneProcessor.connect(silentGain);
  silentGain.connect(audioContext.destination);
  setStreamingAudio(true);
}

function stopLiveMicrophone(keepCall = false) {
  streamingAudio = false;
  microphoneEnabled = false;
  if (!keepCall) liveMode = false;
  if (microphoneProcessor) microphoneProcessor.disconnect();
  if (microphoneSource) microphoneSource.disconnect();
  if (silentGain) silentGain.disconnect();
  microphoneProcessor = null;
  microphoneSource = null;
  silentGain = null;
  if (audioContext) audioContext.close().catch(() => {});
  audioContext = null;
  if (microphoneStream) microphoneStream.getTracks().forEach(track => track.stop());
  microphoneStream = null;
  if (liveSocket) { liveSocket.onclose = null; liveSocket.close(); }
  liveSocket = null;
  updateMicButton();
}

async function start() {
  if (sessionId) return;
  liveMode = true;
  setVoiceState('idle', 'Starting your call…');
  try {
    const result = await api('/api/calls/start');
    sessionId = result.session_id;
    setActive(true);
    add('bot', result.reply.text, result.reply.audio_file, result.reply.audio_files || []);
    try { await prepareLiveMicrophone(); }
    catch (error) { setVoiceState('customer', `Call connected · microphone unavailable: ${error.message}`); }
  } catch (error) {
    stopLiveMicrophone();
    setVoiceState('idle', `Could not start: ${error.message}`);
  }
}

async function toggleMicrophone() {
  if (!sessionId) return;
  if (microphoneEnabled) {
    stopLiveMicrophone(true);
    setVoiceState('customer', 'Microphone off · call is still active');
    return;
  }
  try {
    liveMode = true;
    await prepareLiveMicrophone();
    setStreamingAudio(true);
    setVoiceState('customer', 'Listening…');
  } catch (error) {
    setVoiceState('customer', `Microphone unavailable: ${error.message}`);
  }
}

async function processText(text) {
  const value = String(text || '').trim();
  if (!value || !sessionId || sending) return;
  sending = true;
  setStreamingAudio(false);
  add('customer', value);
  try {
    const result = await api('/api/calls/turn', {session_id: sessionId, text: value});
    add('bot', result.reply.text, result.reply.audio_file, result.reply.audio_files || []);
    if (result.reply.disposition) {
      rememberDisposition(result.reply.disposition);
      stopLiveMicrophone();
      setVoiceState('ended', result.reply.disposition);
      sessionId = null;
      setActive(false);
    }
  } catch (error) {
    $('caption').textContent = `Conversation error: ${error.message}`;
  } finally {
    sending = false;
    if (pendingTranscript && sessionId && !botAudio) {
      const queued = pendingTranscript;
      pendingTranscript = '';
      processText(queued);
    }
  }
}

async function hang() {
  stopAudio();
  stopLiveMicrophone();
  if (sessionId) {
    try {
      const result = await api('/api/calls/hangup', {session_id: sessionId, disposition: 'CxHANG'});
      rememberDisposition(result.disposition || 'CxHANG');
    } catch (error) {
      $('caption').textContent = `Hangup error: ${error.message}`;
    }
  }
  setVoiceState('ended', 'Call ended');
  sessionId = null;
  setActive(false);
}

async function health() {
  try {
    const response = await fetch('/api/health');
    $('health').textContent = (await response.json()).ok ? 'online' : 'offline';
  } catch (_) { $('health').textContent = 'offline'; }
}

async function loadVoices() {
  try {
    const response = await fetch('/api/voices');
    if (!response.ok) throw new Error(await response.text());
    const data = await response.json();
    const select = $('voice-model');
    select.innerHTML = '';
    (data.voices || []).forEach(voice => {
      const option = document.createElement('option');
      option.value = voice.id;
      option.textContent = `${voice.label} · ${voice.clips} clips`;
      select.appendChild(option);
    });
    if (!select.options.length) throw new Error('No complete local voice folders found');
    selectedVoice = data.selected || select.options[0].value;
    select.value = selectedVoice;
    voiceReady = true;
    $('voice-status').textContent = `${data.voices.length} complete local voice${data.voices.length === 1 ? '' : 's'} · choose before a call`;
    select.disabled = Boolean(sessionId);
  } catch (error) {
    $('voice-model').innerHTML = '<option>Voice list unavailable</option>';
    $('voice-status').textContent = error.message;
  }
}

async function changeVoice() {
  const select = $('voice-model');
  const next = select.value;
  if (!next || next === selectedVoice || sessionId) return;
  const previous = selectedVoice;
  select.disabled = true;
  try {
    const result = await api('/api/config/voice', {voice_model: next});
    selectedVoice = result.voice_model;
    $('voice-status').textContent = `Selected ${select.options[select.selectedIndex].textContent}`;
  } catch (error) {
    select.value = previous;
    $('voice-status').textContent = `Voice change failed: ${error.message}`;
  } finally {
    select.disabled = false;
  }
}

$('live').onclick = start;
$('mic-toggle').onclick = toggleMicrophone;
$('hangup').onclick = hang;
$('voice-model').onchange = changeVoice;
health();
loadVoices();
loadLastDisposition();
updateMicButton();

async function refreshStats() {
  try {
    const response = await fetch('/api/stats', {cache: 'no-store'});
    if (!response.ok) return;
    const stats = await response.json();
    $('stat-cpu').textContent = `${Number(stats.cpu_percent || 0).toFixed(1)}%`;
    $('stat-ram').textContent = `${Number(stats.ram_mb || 0).toFixed(0)} MB`;
    $('stat-network').textContent = `${Number(stats.network_kbps || 0).toFixed(1)} KB/s`;
  } catch (_) {
    $('stat-cpu').textContent = '—'; $('stat-ram').textContent = '—'; $('stat-network').textContent = '—';
  }
}
refreshStats();
statsTimer = setInterval(refreshStats, 1000);
