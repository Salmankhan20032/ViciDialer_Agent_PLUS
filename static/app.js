/* ============================================================
   VICIdial AI Live — Gemini Live-Grade Voice Agent (app.js v5)
   Architecture:
   - Browser TTS fires IMMEDIATELY on bot_text (zero-latency)
   - Groq WAV audio arrives via bot_audio (upgrades if still speaking)
   - Strict VAD: hard floor 0.028, 180ms onset debounce, 1800ms echo lockout
   - Barge-in: requires 0.30 rms sustained 150ms (not instant)
   ============================================================ */

'use strict';

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------
const state = {
  callId: null,
  phoneNumber: null,
  callStatus: 'idle',       // idle | active | ended

  // Engine config
  engine: 'gemini',         // 'gemini' (S2S duplex) | 'groq' (turn-based)
  hasGeminiKey: false,
  selectedVoice: 'Aoede',

  // WebSocket
  ws: null,
  pingInterval: null,
  lastPingSentTime: 0,

  // Web Audio & VAD
  audioCtx: null,
  micStream: null,
  micSource: null,
  micAnalyser: null,
  botAnalyser: null,
  mediaRecorder: null,
  audioChunks: [],
  liveProcessor: null,

  // VAD state — tuned for instant response + no self-talk
  isSpeaking: false,
  speechStartTime: 0,
  lastSoundTime: 0,
  silenceThresholdMs: 450,  // 450ms silence = end of utterance
  noiseFloor: 0.018,
  minUtteranceMs: 250,      // accept utterances as short as 250ms
  vadLoopRunning: false,
  isMuted: false,

  // Bot playback & barge-in protection
  isBotSpeaking: false,
  botSpeakingText: '',      // what bot is currently saying (for audio upgrade)
  currentAudio: null,       // HTMLAudioElement for WAV playback
  currentUtterance: null,   // SpeechSynthesisUtterance
  echoLockoutUntil: 0,      // absolute ms timestamp after which mic is live
  bargeInOnsetTime: 0,      // when voice first detected during bot speech
  interruptionShockwave: 0,

  // Canvas visualizer
  canvas: null,
  ctx: null,

  // Token savings & Key Pool telemetry
  audioChunksSent: 0,
  audioChunksGated: 0,
  activeKeyIndex: 1,
  keyPoolData: null,

  // Live Call Timer & Live Call Tokens
  callTimerInterval: null,
  callStartTime: 0,
  callTokens: 0,

  // Quota Countdown
  quotaResetSeconds: 0,
  quotaCountdownInterval: null,

  // Metrics
  metrics: { stt_ms: 0, llm_ms: 0, tts_ms: 0, total_ms: 0 },
};

// ---------------------------------------------------------------------------
// DOM
// ---------------------------------------------------------------------------
const $ = id => document.getElementById(id);
const el = {
  agentStatusText:    $('agent-status-text'),
  agentStatusPill:    $('agent-status-pill'),
  engineBadge:        $('engine-status-badge'),
  voiceSelect:        $('voice-select'),
  liveDispoPill:      $('live-disposition-pill'),
  liveDispoVal:       $('live-disposition-val'),
  pipelineGemini:     $('pipeline-gemini'),
  pipelineGroq:       $('pipeline-groq'),
  btnStart:           $('btn-start-call'),
  btnMute:            $('btn-mute'),
  btnHangup:          $('btn-manual-hangup'),
  orbCanvas:          $('circular-orb-canvas'),
  orbStateTitle:      $('orb-state-title'),
  orbStateSub:        $('orb-state-sub'),
  orbSoundBars:       $('orb-sound-bars'),
  interruptionFlash:  $('interruption-flash'),
  transcriptBox:      $('transcript-container'),
  transcriptEmpty:    $('transcript-empty'),
  liveChatStatus:     $('live-chat-status'),
  activeCallKeyBadge: $('active-call-key-badge'),
  poolHealthBadge:    $('pool-health-badge'),
  poolHealthPct:      $('pool-health-pct'),
  poolHealthDot:      $('pool-health-dot'),
  poolResetCountdown: $('pool-reset-countdown'),
  poolRpm:            $('pool-rpm'),
  poolDaily:          $('pool-daily'),
  poolKeysTrack:      $('pool-keys-track'),
  savingsPct:         $('savings-pct'),
  savingsTokens:      $('savings-tokens'),
  savingsBarFill:     $('savings-bar-fill'),
  stepMic:            $('step-mic'),
  stepGemini:         $('step-gemini'),
  stepSpk:            $('step-spk'),
  stepVad:            $('step-vad'),
  stepStt:            $('step-stt'),
  stepLlm:            $('step-llm'),
  stepTts:            $('step-tts'),
  stepAudio:          $('step-audio'),
  metricLatency:      $('metric-latency'),
  metricStt:          $('metric-stt'),
  metricLlm:          $('metric-llm'),
  metricTts:          $('metric-tts'),
  metricTotal:        $('metric-total'),
  metricPing:         $('metric-ping'),

  // Live Call Timer & Tokens & Key Cooldown Elements
  callTimerWidget:      $('call-timer-widget'),
  callTimerDot:         $('call-timer-dot'),
  callTimerDigits:      $('call-timer-digits'),
  callTimerStatusLabel: $('call-timer-status-label'),
  callTokensWidget:     $('call-tokens-widget'),
  callTokensDigits:     $('call-tokens-digits'),
  poolCooldownList:     $('pool-cooldown-list'),

  // Under-Orb Live Subtitle & WhatsApp Dock Elements
  orbSubtitleWrap:      $('orb-subtitle-wrap'),
  orbLiveSubtitle:      $('orb-live-subtitle'),
  subSpeakerTag:        $('sub-speaker-tag'),
  subSpeakerName:       $('sub-speaker-name'),
  subContent:           $('sub-content'),
  startCaption:         $('start-caption'),
  muteCaption:          $('mute-caption'),
};

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------
document.addEventListener('DOMContentLoaded', () => {
  initCircularOrb();
  bindControls();
  loadConfig();
});

async function loadConfig() {
  try {
    const res = await fetch('/api/config');
    if (res.ok) {
      const cfg = await res.json();
      state.engine = cfg.voice_engine || 'gemini';
      state.hasGeminiKey = cfg.has_gemini_key;
      if (cfg.gemini_voice) {
        state.selectedVoice = cfg.gemini_voice;
        if (el.voiceSelect) el.voiceSelect.value = cfg.gemini_voice;
      }
      if (cfg.key_pool) {
        updateKeyPoolUI(cfg.key_pool);
      }
      updateEngineUI();
    }
  } catch (err) {
    console.warn('Could not load config:', err);
  }
}

let keyCooldownInterval = null;

function formatQuotaCountdown(totalSec) {
  if (!totalSec || totalSec <= 0) return '00h 00m 00s';
  const hours = Math.floor(totalSec / 3600);
  const minutes = Math.floor((totalSec % 3600) / 60);
  const seconds = totalSec % 60;
  return `${hours}h ${String(minutes).padStart(2, '0')}m ${String(seconds).padStart(2, '0')}s`;
}

function startQuotaCountdownTimer() {
  if (el.poolResetCountdown) {
    el.poolResetCountdown.textContent = formatQuotaCountdown(state.quotaResetSeconds);
  }
  if (state.quotaCountdownInterval) return;

  state.quotaCountdownInterval = setInterval(() => {
    if (state.quotaResetSeconds > 0) {
      state.quotaResetSeconds--;
      if (el.poolResetCountdown) {
        el.poolResetCountdown.textContent = formatQuotaCountdown(state.quotaResetSeconds);
      }
    } else {
      if (el.poolResetCountdown) {
        el.poolResetCountdown.textContent = 'REFRESHING...';
      }
      clearInterval(state.quotaCountdownInterval);
      state.quotaCountdownInterval = null;
      fetchKeyPoolStats();
    }
  }, 1000);
}

function updateKeyPoolUI(poolData) {
  if (!poolData) return;
  state.keyPoolData = poolData;

  if (el.poolRpm) el.poolRpm.textContent = poolData.rpm_capacity || (poolData.total_keys * 15);
  if (el.poolDaily) el.poolDaily.textContent = (poolData.daily_capacity || (poolData.total_keys * 1500)).toLocaleString();

  // Health % & Dot Telemetry
  const healthPct = (typeof poolData.health_pct === 'number')
    ? poolData.health_pct
    : (poolData.total_keys > 0 ? Math.round((poolData.healthy_keys / poolData.total_keys) * 100) : 100);

  if (el.poolHealthPct) {
    el.poolHealthPct.textContent = `${healthPct}%`;
  }
  if (el.poolHealthDot) {
    if (healthPct >= 100) {
      el.poolHealthDot.style.background = '#10b981';
      el.poolHealthDot.style.boxShadow = '0 0 8px rgba(16, 185, 129, 0.7)';
    } else if (healthPct >= 50) {
      el.poolHealthDot.style.background = '#f59e0b';
      el.poolHealthDot.style.boxShadow = '0 0 8px rgba(245, 158, 11, 0.7)';
    } else {
      el.poolHealthDot.style.background = '#ef4444';
      el.poolHealthDot.style.boxShadow = '0 0 8px rgba(239, 68, 68, 0.7)';
    }
  }

  // Real-time Quota Reset Countdown (hours, minutes, seconds left)
  if (typeof poolData.quota_reset_seconds === 'number') {
    state.quotaResetSeconds = poolData.quota_reset_seconds;
    startQuotaCountdownTimer();
  }

  if (el.poolHealthBadge) {
    if (poolData.cooling_keys > 0) {
      el.poolHealthBadge.textContent = `${poolData.healthy_keys}/${poolData.total_keys} ACTIVE`;
      el.poolHealthBadge.style.color = 'var(--amber)';
      el.poolHealthBadge.style.borderColor = 'rgba(245, 158, 11, 0.5)';
      el.poolHealthBadge.style.background = 'rgba(245, 158, 11, 0.15)';
    } else {
      el.poolHealthBadge.textContent = '100% HEALTHY';
      el.poolHealthBadge.style.color = '#6ee7b7';
      el.poolHealthBadge.style.borderColor = 'rgba(16, 185, 129, 0.4)';
      el.poolHealthBadge.style.background = 'rgba(16, 185, 129, 0.15)';
    }
  }

  if (el.poolKeysTrack && poolData.keys && poolData.keys.length > 0) {
    el.poolKeysTrack.innerHTML = '';
    poolData.keys.forEach(k => {
      const node = document.createElement('span');
      node.className = `key-node ${k.is_healthy ? 'active' : 'cooling'}`;
      if (state.callStatus === 'active' && k.index === state.activeKeyIndex) {
        node.classList.add('in-call');
      }
      node.id = `key-node-${k.index}`;
      node.title = `Key #${k.index} (${k.masked_key}) — ${k.total_calls} calls, ${k.failures} errors${k.seconds_left > 0 ? ` (cooldown: ${k.seconds_left}s remaining)` : ''}`;
      node.textContent = `K${k.index}`;
      el.poolKeysTrack.appendChild(node);
    });
  }

  renderKeyProgressSliders();
}

function renderKeyProgressSliders() {
  if (!el.poolCooldownList || !state.keyPoolData || !state.keyPoolData.keys) return;

  const coolingKeys = state.keyPoolData.keys.filter(k => !k.is_healthy || (k.seconds_left && k.seconds_left > 0));

  if (coolingKeys.length === 0) {
    el.poolCooldownList.innerHTML = '';
    if (keyCooldownInterval) {
      clearInterval(keyCooldownInterval);
      keyCooldownInterval = null;
    }
    return;
  }

  let html = '';
  coolingKeys.forEach(k => {
    const totalDuration = k.cooldown_duration || 45;
    const remaining = Math.max(0, k.seconds_left || 0);
    const pct = Math.min(100, Math.max(0, Math.round((remaining / totalDuration) * 100)));
    html += `
      <div class="key-progress-item cooling" id="kp-item-${k.index}">
        <div class="key-progress-header">
          <span class="kp-key-name">🔑 Key #${k.index} (${k.masked_key})</span>
          <span class="kp-timer" id="kp-timer-${k.index}">⏳ Available in ${remaining}s</span>
        </div>
        <div class="kp-slider-track">
          <div class="kp-slider-fill" id="kp-bar-${k.index}" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  });
  el.poolCooldownList.innerHTML = html;

  if (!keyCooldownInterval) {
    keyCooldownInterval = setInterval(tickKeyCooldowns, 1000);
  }
}

function tickKeyCooldowns() {
  if (!state.keyPoolData || !state.keyPoolData.keys) return;
  let hasActiveCooldowns = false;

  state.keyPoolData.keys.forEach(k => {
    if (k.seconds_left && k.seconds_left > 0) {
      k.seconds_left--;
      hasActiveCooldowns = true;

      const timerEl = document.getElementById(`kp-timer-${k.index}`);
      const barEl = document.getElementById(`kp-bar-${k.index}`);
      if (timerEl) {
        if (k.seconds_left > 0) {
          timerEl.textContent = `⏳ Available in ${k.seconds_left}s`;
        } else {
          timerEl.textContent = `✅ Ready!`;
          k.is_healthy = true;
          const node = document.getElementById(`key-node-${k.index}`);
          if (node) {
            node.classList.remove('cooling');
            node.classList.add('active');
          }
        }
      }
      if (barEl) {
        const totalDuration = k.cooldown_duration || 45;
        const pct = Math.min(100, Math.max(0, Math.round((k.seconds_left / totalDuration) * 100)));
        barEl.style.width = `${pct}%`;
      }
    }
  });

  if (!hasActiveCooldowns) {
    if (keyCooldownInterval) {
      clearInterval(keyCooldownInterval);
      keyCooldownInterval = null;
    }
    fetchKeyPoolStats();
  }
}

async function fetchKeyPoolStats() {
  try {
    const res = await fetch('/api/key_pool');
    if (res.ok) {
      const data = await res.json();
      updateKeyPoolUI(data);
    }
  } catch (_) {}
}

// ---------------------------------------------------------------------------
// Live Call Timer & Tokens Controller
// ---------------------------------------------------------------------------
function updateTokensUI() {
  if (el.callTokensDigits) {
    el.callTokensDigits.textContent = state.callTokens.toLocaleString();
  }
}

function recordCallTokens(tokensToAdd) {
  if (state.callStatus !== 'active') return;
  state.callTokens += tokensToAdd;
  updateTokensUI();
}

function startCallTimer() {
  stopCallTimer();
  state.callStartTime = Date.now();
  state.callTokens = 180; // Baseline context + system prompt token estimate
  updateTokensUI();

  if (el.callTokensWidget) el.callTokensWidget.classList.add('active');
  if (el.callTimerWidget) el.callTimerWidget.classList.add('active');
  if (el.callTimerDot) el.callTimerDot.classList.add('active');
  if (el.callTimerStatusLabel) el.callTimerStatusLabel.textContent = 'CALL IN PROGRESS';
  if (el.callTimerDigits) el.callTimerDigits.textContent = '00:00';

  state.callTimerInterval = setInterval(() => {
    if (state.callStatus !== 'active') {
      stopCallTimer();
      return;
    }
    const elapsedSec = Math.floor((Date.now() - state.callStartTime) / 1000);
    const mins = String(Math.floor(elapsedSec / 60)).padStart(2, '0');
    const secs = String(elapsedSec % 60).padStart(2, '0');
    if (el.callTimerDigits) {
      el.callTimerDigits.textContent = `${mins}:${secs}`;
    }

    // Bidirectional audio token accumulation:
    // Gemini 2.5 Flash Native Audio is ~25 tok/sec for audio stream
    const audioTokenDelta = state.isSpeaking ? 28 : (state.isBotSpeaking ? 32 : 22);
    state.callTokens += audioTokenDelta;
    updateTokensUI();
  }, 1000);
}

function stopCallTimer() {
  if (state.callTimerInterval) {
    clearInterval(state.callTimerInterval);
    state.callTimerInterval = null;
  }
  if (el.callTimerWidget) el.callTimerWidget.classList.remove('active');
  if (el.callTimerDot) el.callTimerDot.classList.remove('active');
  if (el.callTimerStatusLabel) el.callTimerStatusLabel.textContent = 'CALL ENDED';
  if (el.callTokensWidget) el.callTokensWidget.classList.remove('active');
}

function resetCallTimer() {
  stopCallTimer();
  state.callTokens = 0;
  updateTokensUI();
  if (el.callTimerDigits) el.callTimerDigits.textContent = '00:00';
  if (el.callTimerStatusLabel) el.callTimerStatusLabel.textContent = 'CALL DURATION';
  if (el.callTokensWidget) el.callTokensWidget.classList.remove('active');
}

function updateSavingsUI() {
  const total = state.audioChunksSent + state.audioChunksGated;
  if (total === 0) return;
  const pct = Math.min(100, Math.round((state.audioChunksGated / total) * 100));
  const estTokens = Math.round(state.audioChunksGated * 1.075);

  if (el.savingsPct) el.savingsPct.textContent = `${pct}%`;
  if (el.savingsTokens) el.savingsTokens.textContent = `(${estTokens.toLocaleString()} tokens saved)`;
  if (el.savingsBarFill) el.savingsBarFill.style.width = `${pct}%`;
}

function updateEngineUI() {
  if (state.engine === 'gemini') {
    if (el.engineBadge) {
      if (state.hasGeminiKey) {
        el.engineBadge.textContent = '⚡ GEMINI LIVE (S2S)';
        el.engineBadge.style.borderColor = 'var(--cyan)';
      } else {
        el.engineBadge.textContent = '⚠️ GEMINI KEY MISSING';
        el.engineBadge.style.borderColor = 'var(--amber)';
      }
    }
    if (el.pipelineGemini) el.pipelineGemini.classList.remove('hidden');
    if (el.pipelineGroq) el.pipelineGroq.classList.add('hidden');
  } else {
    if (el.engineBadge) {
      el.engineBadge.textContent = 'GROQ (Turn-Based)';
      el.engineBadge.style.borderColor = 'rgba(255, 255, 255, 0.2)';
    }
    if (el.pipelineGemini) el.pipelineGemini.classList.add('hidden');
    if (el.pipelineGroq) el.pipelineGroq.classList.remove('hidden');
  }
}

// ---------------------------------------------------------------------------
// 60FPS Canvas Voice Orb
// ---------------------------------------------------------------------------
function initCircularOrb() {
  state.canvas = el.orbCanvas;
  if (!state.canvas) return;
  state.ctx = state.canvas.getContext('2d');
  requestAnimationFrame(renderOrbLoop);
}

function renderOrbLoop() {
  const ctx = state.ctx;
  const cvs = state.canvas;
  const w = cvs.width;
  const h = cvs.height;
  const cx = w / 2;
  const cy = h / 2;
  const baseRadius = 112;
  const time = Date.now() * 0.0028;

  ctx.clearRect(0, 0, w, h);
  drawRadialTeeth(ctx, cx, cy, baseRadius, time);
  drawFluidRibbons(ctx, cx, cy, baseRadius, time);
  drawCoreCircle(ctx, cx, cy, baseRadius);

  if (state.interruptionShockwave > 0) {
    drawShockwave(ctx, cx, cy, baseRadius);
    state.interruptionShockwave -= 0.022;
  }

  requestAnimationFrame(renderOrbLoop);
}

function drawRadialTeeth(ctx, cx, cy, baseRadius, time) {
  const numBars = 64;
  const angleStep = (Math.PI * 2) / numBars;

  let audioData = null;
  if (state.isBotSpeaking && state.botAnalyser) {
    audioData = new Uint8Array(state.botAnalyser.frequencyBinCount);
    state.botAnalyser.getByteFrequencyData(audioData);
  } else if (state.isSpeaking && state.micAnalyser) {
    audioData = new Uint8Array(state.micAnalyser.frequencyBinCount);
    state.micAnalyser.getByteFrequencyData(audioData);
  }

  ctx.save();
  ctx.lineWidth = 2.4;

  for (let i = 0; i < numBars; i++) {
    const angle = i * angleStep;
    let amp = 0.2;

    if (audioData) {
      const idx = Math.floor((i / numBars) * (audioData.length / 2));
      amp = Math.max(0.12, (audioData[idx] || 0) / 255);
    } else if (state.isBotSpeaking) {
      const syllable = Math.abs(Math.sin(time * 6.5) * Math.cos(time * 2.8));
      amp = 0.22 + syllable * 0.58 + Math.sin(angle * 5 + time * 9) * 0.12;
    } else if (state.callStatus === 'active') {
      // Idle breathing (waiting for user)
      amp = 0.14 + Math.sin(angle * 3 + time * 1.5) * 0.06;
    } else {
      amp = 0.18 + Math.sin(angle * 3 + time * 2) * 0.08;
    }

    const waveEnvelope = Math.sin(angle * 4 + time * 1.5) * 5 + Math.cos(angle * 2 - time * 0.8) * 4;
    const barHeight = Math.max(6, 12 + waveEnvelope + amp * 46);
    const rStart = baseRadius + 20;
    const rEnd = rStart + barHeight;

    const x1 = cx + Math.cos(angle) * rStart;
    const y1 = cy + Math.sin(angle) * rStart;
    const x2 = cx + Math.cos(angle) * rEnd;
    const y2 = cy + Math.sin(angle) * rEnd;

    let strokeColor;
    if (state.isSpeaking) {
      strokeColor = (Math.cos(angle) < 0) ? '#10b981' : '#f59e0b';
    } else {
      const t = (Math.cos(angle) + 1) / 2;
      strokeColor = t < 0.45 ? '#00f0ff' : t > 0.55 ? '#a855f7' : '#6366f1';
    }

    ctx.strokeStyle = strokeColor;
    ctx.shadowColor = strokeColor;
    ctx.shadowBlur = 9;
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
  }

  ctx.restore();
}

function drawFluidRibbons(ctx, cx, cy, baseRadius, time) {
  const points = 72;
  const angleStep = (Math.PI * 2) / points;

  let audioData = null;
  if (state.isBotSpeaking && state.botAnalyser) {
    audioData = new Uint8Array(state.botAnalyser.frequencyBinCount);
    state.botAnalyser.getByteFrequencyData(audioData);
  } else if (state.isSpeaking && state.micAnalyser) {
    audioData = new Uint8Array(state.micAnalyser.frequencyBinCount);
    state.micAnalyser.getByteFrequencyData(audioData);
  }

  ctx.save();
  ctx.lineWidth = 3.6;
  ctx.beginPath();
  for (let i = 0; i <= points; i++) {
    const angle = i * angleStep;
    let val = 0.2;
    if (audioData) {
      const idx = Math.floor((i / points) * (audioData.length / 3));
      val = (audioData[idx] || 0) / 255;
    } else if (state.isBotSpeaking) {
      val = 0.32 + Math.abs(Math.sin(time * 5.5 + angle * 2)) * 0.48;
    }
    const wave = Math.sin(angle * 5 + time * 3.2) * (7 + val * 18) + Math.cos(angle * 3 - time * 2) * 5;
    const r = baseRadius + 14 + wave;
    const x = cx + Math.cos(angle) * r;
    const y = cy + Math.sin(angle) * r;
    if (i === 0) ctx.moveTo(x, y);
    else ctx.lineTo(x, y);
  }
  ctx.closePath();

  const grad1 = ctx.createLinearGradient(cx - baseRadius - 20, cy, cx + baseRadius + 20, cy);
  if (state.isSpeaking) {
    grad1.addColorStop(0, '#10b981');
    grad1.addColorStop(1, '#f59e0b');
    ctx.shadowColor = '#10b981';
  } else {
    grad1.addColorStop(0, '#00f0ff');
    grad1.addColorStop(0.5, '#6366f1');
    grad1.addColorStop(1, '#a855f7');
    ctx.shadowColor = '#00f0ff';
  }
  ctx.strokeStyle = grad1;
  ctx.shadowBlur = 20;
  ctx.stroke();

  ctx.lineWidth = 2.0;
  ctx.beginPath();
  for (let i = 0; i <= points; i++) {
    const angle = i * angleStep;
    const wave2 = Math.sin(angle * 6 - time * 2.8) * 4 + Math.cos(angle * 4 + time * 1.8) * 3;
    const r = baseRadius + 6 + wave2;
    const x = cx + Math.cos(angle) * r;
    const y = cy + Math.sin(angle) * r;
    if (i === 0) ctx.moveTo(x, y);
    else ctx.lineTo(x, y);
  }
  ctx.closePath();

  const grad2 = ctx.createLinearGradient(cx - baseRadius, cy, cx + baseRadius, cy);
  if (state.isSpeaking) {
    grad2.addColorStop(0, 'rgba(16, 185, 129, 0.7)');
    grad2.addColorStop(1, 'rgba(245, 158, 11, 0.7)');
  } else {
    grad2.addColorStop(0, 'rgba(0, 240, 255, 0.75)');
    grad2.addColorStop(1, 'rgba(168, 85, 247, 0.75)');
  }
  ctx.strokeStyle = grad2;
  ctx.shadowBlur = 12;
  ctx.stroke();

  ctx.lineWidth = 1.6;
  ctx.strokeStyle = 'rgba(255, 255, 255, 0.25)';
  ctx.shadowBlur = 6;
  ctx.beginPath();
  ctx.arc(cx, cy, baseRadius + 5, 0, Math.PI * 2);
  ctx.stroke();
  ctx.restore();
}

function drawCoreCircle(ctx, cx, cy, baseRadius) {
  ctx.save();
  const grad = ctx.createRadialGradient(cx, cy, 10, cx, cy, baseRadius);
  grad.addColorStop(0, 'rgba(14, 20, 34, 0.98)');
  grad.addColorStop(0.85, 'rgba(7, 10, 18, 0.95)');
  grad.addColorStop(1, 'rgba(0, 240, 255, 0.2)');
  ctx.fillStyle = grad;
  ctx.beginPath();
  ctx.arc(cx, cy, baseRadius, 0, Math.PI * 2);
  ctx.fill();
  ctx.strokeStyle = 'rgba(0, 240, 255, 0.45)';
  ctx.lineWidth = 1.8;
  ctx.shadowColor = 'rgba(0, 240, 255, 0.5)';
  ctx.shadowBlur = 10;
  ctx.stroke();
  ctx.restore();
}

function drawShockwave(ctx, cx, cy, baseRadius) {
  const progress = 1 - state.interruptionShockwave;
  const ringRadius = baseRadius + progress * 110;
  const alpha = state.interruptionShockwave;
  ctx.save();
  ctx.shadowColor = '#f43f5e';
  ctx.shadowBlur = 28;
  ctx.strokeStyle = `rgba(244, 63, 94, ${alpha})`;
  ctx.lineWidth = 3.5;
  ctx.beginPath();
  ctx.arc(cx, cy, ringRadius, 0, Math.PI * 2);
  ctx.stroke();
  ctx.restore();
}

// ---------------------------------------------------------------------------
// Telemetry HUD
// ---------------------------------------------------------------------------
const PIPELINE_STEPS = ['mic', 'gemini', 'spk', 'vad', 'stt', 'llm', 'tts', 'audio'];

function activatePipelineStep(activeStep) {
  PIPELINE_STEPS.forEach(step => {
    const elStep = $(`step-${step}`);
    if (elStep) {
      elStep.classList.toggle('active', step === activeStep);
      elStep.classList.toggle('active-highlight', step === activeStep);
    }
  });
}

function updateLatencyMetrics(metrics) {
  if (!metrics) return;
  if (metrics.stt_ms !== undefined && metrics.stt_ms > 0 && el.metricStt)     el.metricStt.textContent   = `${metrics.stt_ms}ms`;
  if (metrics.llm_ms !== undefined && metrics.llm_ms > 0 && el.metricLlm)     el.metricLlm.textContent   = `${metrics.llm_ms}ms`;
  if (metrics.tts_ms !== undefined && metrics.tts_ms > 0 && el.metricTts)     el.metricTts.textContent   = `${metrics.tts_ms}ms`;
  if (metrics.total_ms !== undefined && metrics.total_ms > 0 && el.metricTotal) el.metricTotal.textContent = `${metrics.total_ms}ms`;
}

function startPingTimer() {
  if (state.pingInterval) clearInterval(state.pingInterval);
  state.pingInterval = setInterval(() => {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) {
      state.lastPingSentTime = Date.now();
      sendWS({ type: 'ping', format: String(state.lastPingSentTime) });
    }
  }, 4000);
}

// ---------------------------------------------------------------------------
// Controls
// ---------------------------------------------------------------------------
function bindControls() {
  el.btnStart.addEventListener('click', startLiveCall);
  el.btnMute.addEventListener('click', toggleMute);
  el.btnHangup.addEventListener('click', onManualHangup);
  if (el.voiceSelect) {
    el.voiceSelect.addEventListener('change', e => {
      state.selectedVoice = e.target.value;
      console.log('Voice selected:', state.selectedVoice);
    });
  }
  if (el.orbCanvas) {
    el.orbCanvas.style.cursor = 'pointer';
    el.orbCanvas.title = 'Click to interrupt Alex while speaking';
    el.orbCanvas.addEventListener('click', () => {
      if (state.callStatus === 'active' && state.isBotSpeaking) {
        triggerBargeIn();
      }
    });
  }
}

// ---------------------------------------------------------------------------
// WebSocket Lifecycle
// ---------------------------------------------------------------------------
function connectWebSocket() {
  return new Promise((resolve, reject) => {
    if (state.ws && state.ws.readyState === WebSocket.OPEN) {
      resolve(state.ws);
      return;
    }
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    state.ws = new WebSocket(`${proto}//${location.host}/ws/call`);

    state.ws.onopen = () => {
      console.log('✅ WebSocket connected');
      startPingTimer();
      resolve(state.ws);
    };
    state.ws.onerror = err => {
      console.error('❌ WebSocket error:', err);
      if (state.callStatus === 'active') {
        state.callStatus = 'ended';
        teardownAudioPipeline();
        stopCallTimer();
        stopCurrentPlayback();
        hideOrbSubtitle();
      }
      reject(err);
    };
    state.ws.onclose = () => {
      console.log('ℹ️ WebSocket closed');
      if (state.pingInterval) { clearInterval(state.pingInterval); state.pingInterval = null; }
      if (state.callStatus === 'active') {
        state.callStatus = 'ended';
        teardownAudioPipeline();
        stopCallTimer();
        stopCurrentPlayback();
        hideOrbSubtitle();
        const micOnSvg = el.btnMute ? el.btnMute.querySelector('.icon-mic') : null;
        const micOffSvg = el.btnMute ? el.btnMute.querySelector('.icon-mic-off') : null;
        if (micOnSvg) micOnSvg.classList.remove('hidden');
        if (micOffSvg) micOffSvg.classList.add('hidden');
        if (el.btnMute) el.btnMute.disabled = true;
        if (el.btnHangup) el.btnHangup.disabled = true;
        if (el.btnStart) el.btnStart.disabled = false;
        if (el.startCaption) el.startCaption.textContent = 'Start Call';
        if (el.agentStatusText) el.agentStatusText.textContent = 'Disconnected';
        setOrbState('CALL', 'DISCONNECTED');
      }
    };
    state.ws.onmessage = e => {
      try { handleWSMessage(JSON.parse(e.data)); } catch (err) { console.error('WS parse error:', err); }
    };
  });
}

function sendWS(msg) {
  if (state.ws && state.ws.readyState === WebSocket.OPEN) {
    state.ws.send(JSON.stringify(msg));
  }
}

// ---------------------------------------------------------------------------
// Start Live Call
// ---------------------------------------------------------------------------
async function startLiveCall() {
  if (state.engine === 'gemini' && !state.hasGeminiKey) {
    alert('Please add GEMINI_API_KEY to your .env file to use Gemini Live voice!\n\nYou can get a free key in 30 seconds at https://aistudio.google.com/');
    return;
  }

  // Unlock browser audio on user gesture
  if ('speechSynthesis' in window) {
    try { window.speechSynthesis.cancel(); window.speechSynthesis.resume(); } catch (_) {}
  }

  if (state.callStatus === 'active') {
    onManualHangup();
    await new Promise(r => setTimeout(r, 400));
  }

  if (!livePlaybackCtx) {
    livePlaybackCtx = new (window.AudioContext || window.webkitAudioContext)();
  }
  if (livePlaybackCtx.state === 'suspended') {
    try { await livePlaybackCtx.resume(); } catch (_) {}
  }

  teardownAudioPipeline();
  stopCurrentPlayback();
  clearTranscript();
  resetCallTimer();
  hideOrbSubtitle();
  state.isTerminalCall = false;
  state.isInitialGreetingLock = true;
  setOrbState(state.agentName || 'ALEX', 'CONNECTING...');
  el.agentStatusText.textContent = 'Connecting…';
  activatePipelineStep(state.engine === 'gemini' ? 'gemini' : 'vad');
  el.btnStart.disabled = true;
  if (el.startCaption) el.startCaption.textContent = 'Calling…';

  try {
    await setupAudioPipeline();
    await connectWebSocket();

    sendWS({
      type: 'start_call',
      engine: state.engine,
      voice: state.selectedVoice || 'Aoede',
    });

    state.callStatus = 'active';
    el.btnMute.disabled = false;
    el.btnHangup.disabled = false;
    if (el.startCaption) el.startCaption.textContent = 'Connected';
    el.agentStatusText.textContent = `Active (${state.engine === 'gemini' ? 'S2S Duplex' : 'Groq'})`;

    if (state.engine === 'gemini') {
      setupLivePCMStreaming();
    }

  } catch (err) {
    console.error('Failed to start live call:', err);
    teardownAudioPipeline();
    setOrbState('MIC', 'DENIED');
    el.agentStatusText.textContent = 'Mic Error — allow access';
    el.btnStart.disabled = false;
    if (el.startCaption) el.startCaption.textContent = 'Start Call';
  }
}

// ---------------------------------------------------------------------------
// Audio Pipeline Setup & Teardown
// ---------------------------------------------------------------------------
async function setupAudioPipeline() {
  if (!state.audioCtx) {
    state.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  }
  if (state.audioCtx.state === 'suspended') {
    await state.audioCtx.resume();
  }

  if (!state.micStream) {
    state.micStream = await navigator.mediaDevices.getUserMedia({
      audio: {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl:  true,
        channelCount:     1,
        sampleRate:       16000,
      },
      video: false,
    });
  }

  state.micSource  = state.audioCtx.createMediaStreamSource(state.micStream);
  state.micAnalyser = state.audioCtx.createAnalyser();
  state.micAnalyser.fftSize = 1024;
  state.micAnalyser.smoothingTimeConstant = 0.5;
  state.micSource.connect(state.micAnalyser);

  state.botAnalyser = state.audioCtx.createAnalyser();
  state.botAnalyser.fftSize = 512;

  if (state.engine === 'groq') {
    startVADLoop();
  }
}

function teardownAudioPipeline() {
  teardownLivePCMStreaming();

  if (state.greetingLockTimeout) {
    clearTimeout(state.greetingLockTimeout);
    state.greetingLockTimeout = null;
  }
  state.isInitialGreetingLock = false;

  if (state.micStream) {
    state.micStream.getTracks().forEach(track => {
      try {
        track.stop();
        console.log('🔇 Mic hardware track stopped — recording indicator off');
      } catch (err) {
        console.error('Error stopping mic track:', err);
      }
    });
    state.micStream = null;
  }

  if (state.micSource) {
    try { state.micSource.disconnect(); } catch (_) {}
    state.micSource = null;
  }
  if (state.micAnalyser) {
    try { state.micAnalyser.disconnect(); } catch (_) {}
    state.micAnalyser = null;
  }
}

// ---------------------------------------------------------------------------
// Spectral Voice Activity Detector
// Returns 0.0–1.0: ratio of energy in voice band (300–3400 Hz) vs total.
// Real speech scores > 0.55. Noise/fans/keyboards score < 0.40.
// ---------------------------------------------------------------------------
function getVoiceScore(freqData, sampleRate) {
  const binCount   = freqData.length; // = fftSize / 2
  const binHz      = (sampleRate / 2) / binCount;

  const voiceLow   = 300;   // Hz — fundamental vowel frequencies
  const voiceHigh  = 3400;  // Hz — top of telephone speech band

  const voiceLowBin  = Math.floor(voiceLow  / binHz);
  const voiceHighBin = Math.min(binCount - 1, Math.ceil(voiceHigh / binHz));

  let totalEnergy = 0;
  let voiceEnergy = 0;

  for (let i = 1; i < binCount; i++) {  // skip DC bin
    const e = (freqData[i] / 255) ** 2;
    totalEnergy += e;
    if (i >= voiceLowBin && i <= voiceHighBin) voiceEnergy += e;
  }

  if (totalEnergy < 0.0001) return 0; // pure silence
  return voiceEnergy / totalEnergy;
}

// ---------------------------------------------------------------------------
// VAD Engine — spectral + energy dual-gate
// ---------------------------------------------------------------------------
function startVADLoop() {
  if (state.vadLoopRunning) return;
  state.vadLoopRunning = true;

  const timeData = new Uint8Array(state.micAnalyser.fftSize);
  const freqData = new Uint8Array(state.micAnalyser.frequencyBinCount);
  const sampleRate = state.audioCtx.sampleRate;

  // Smoothed RMS to dampen single-frame spikes
  let smoothedRms = 0;

  function vadTick() {
    if (state.callStatus !== 'active') {
      state.vadLoopRunning = false;
      return;
    }

    if (!state.isMuted) {
      // ── Time-domain RMS (energy gate) ──
      state.micAnalyser.getByteTimeDomainData(timeData);
      let sum = 0;
      for (let i = 0; i < timeData.length; i++) {
        const norm = (timeData[i] - 128) / 128;
        sum += norm * norm;
      }
      const rawRms = Math.sqrt(sum / timeData.length);
      // Smooth RMS: fast attack (0.4), slow release (0.08) — mimics a compressor
      smoothedRms = rawRms > smoothedRms
        ? smoothedRms * 0.60 + rawRms * 0.40   // fast attack
        : smoothedRms * 0.92 + rawRms * 0.08;  // slow release
      const rms = smoothedRms;
      const now = Date.now();

      // ── 1. Echo lockout ──
      if (now < state.echoLockoutUntil) {
        requestAnimationFrame(vadTick);
        return;
      }

      // ── Spectral voice score (frequency-domain gate) ──
      state.micAnalyser.getByteFrequencyData(freqData);
      const voiceScore = getVoiceScore(freqData, sampleRate);
      // voiceScore > 0.42 = speech-like; < 0.35 = noise/fan/keyboard
      const isSpeechLike = voiceScore > 0.42;

      // ── 2. Bot speaking: barge-in requires BOTH energy AND voice spectrum ──
      if (state.isBotSpeaking) {
        if (rms > 0.15 && isSpeechLike) {
          if (state.bargeInOnsetTime === 0) {
            state.bargeInOnsetTime = now;
          } else if (now - state.bargeInOnsetTime >= 100) {
            state.bargeInOnsetTime = 0;
            triggerBargeIn();
          }
        } else {
          state.bargeInOnsetTime = 0;
        }
        requestAnimationFrame(vadTick);
        return;
      }

      // ── 3. Normal VAD: energy + spectral dual-gate ──
      if (!state.isSpeaking) {
        // Noise floor adapts toward current RMS (fast rise, slow fall)
        if (rms > state.noiseFloor) {
          state.noiseFloor = state.noiseFloor * 0.85 + rms * 0.15; // fast rise
        } else {
          state.noiseFloor = state.noiseFloor * 0.98 + rms * 0.02; // slow fall
        }
      }

      // Dynamic threshold: at least 0.026 hard floor, and 2.8× noise floor
      const threshold = Math.max(0.026, state.noiseFloor * 2.8);

      // Must pass BOTH: energy threshold AND voice-like spectrum
      const isSpeech = rms > threshold && isSpeechLike;

      if (isSpeech) {
        state.lastSoundTime = now;

        if (!state.isSpeaking) {
          state.isSpeaking      = true;
          state.speechStartTime = now;
          activatePipelineStep('vad');
          setOrbState('YOU', 'SPEAKING');
          startUtteranceRecording();
        }
      } else {
        if (state.isSpeaking) {
          const silenceDuration   = now - state.lastSoundTime;
          const utteranceDuration = now - state.speechStartTime;

          if (silenceDuration > state.silenceThresholdMs && utteranceDuration >= state.minUtteranceMs) {
            state.isSpeaking = false;
            setOrbState('ALEX', 'THINKING...');
            activatePipelineStep('stt');
            stopUtteranceRecordingAndSend();
          }
        }
      }
    }

    requestAnimationFrame(vadTick);
  }

  requestAnimationFrame(vadTick);
}

// ---------------------------------------------------------------------------
// Barge-In
// ---------------------------------------------------------------------------
function triggerBargeIn() {
  console.log('⚡ BARGE-IN: User interrupting bot!');
  stopCurrentPlayback();
  state.isBotSpeaking    = false;
  state.botSpeakingText  = '';
  state.echoLockoutUntil = 0;
  state.bargeInOnsetTime = 0;

  state.interruptionShockwave = 1.0;
  el.interruptionFlash.classList.remove('hidden');
  setTimeout(() => el.interruptionFlash.classList.add('hidden'), 1800);

  setOrbState('YOU', 'SPEAKING');
  activatePipelineStep('vad');
  sendWS({ type: 'interrupt' });
}

function stopCurrentPlayback() {
  stopLivePlaybackImmediately();
  // Stop WAV audio
  if (state.currentAudio) {
    try { state.currentAudio.pause(); state.currentAudio.currentTime = 0; } catch (_) {}
    state.currentAudio = null;
  }
  // Stop browser TTS
  if ('speechSynthesis' in window) {
    try { window.speechSynthesis.cancel(); } catch (_) {}
  }
  state.currentUtterance = null;
  state.isBotSpeaking    = false;
  state.botSpeakingText  = '';
  state.echoLockoutUntil = Date.now() + 300;
}

// ---------------------------------------------------------------------------
// Utterance Recording
// ---------------------------------------------------------------------------
function startUtteranceRecording() {
  try {
    state.audioChunks  = [];
    const mime         = getSupportedMimeType();
    state.mediaRecorder = new MediaRecorder(state.micStream, { mimeType: mime });
    state.mediaRecorder.ondataavailable = e => {
      if (e.data && e.data.size > 0) state.audioChunks.push(e.data);
    };
    state.mediaRecorder.start(50);
  } catch (err) {
    console.error('MediaRecorder start error:', err);
  }
}

function stopUtteranceRecordingAndSend() {
  if (!state.mediaRecorder || state.mediaRecorder.state !== 'recording') return;

  state.mediaRecorder.onstop = async () => {
    const mime = getSupportedMimeType();
    const blob = new Blob(state.audioChunks, { type: mime });

    // Drop tiny blobs — breathing, pops, keyboard taps (< 1500 bytes)
    if (blob.size < 1500) {
      if (state.callStatus === 'active') {
        activatePipelineStep('vad');
        setOrbState('ALEX', 'LISTENING...');
      }
      return;
    }

    activatePipelineStep('stt');
    setOrbState('ALEX', 'THINKING...');

    const ext = mime.includes('webm') ? 'webm'
              : mime.includes('ogg')  ? 'ogg'
              : mime.includes('mp4')  ? 'mp4'
              : 'wav';

    const reader = new FileReader();
    reader.onloadend = () => {
      const b64 = reader.result.split(',')[1];
      sendWS({ type: 'audio_turn', audio_base64: b64, format: ext });
    };
    reader.readAsDataURL(blob);
  };

  state.mediaRecorder.stop();
}

// ---------------------------------------------------------------------------
// Gemini Live: Real-Time Audio Streaming (PCM 16kHz in, PCM 24kHz out)
// ---------------------------------------------------------------------------

function floatTo16BitPCM(input) {
  const output = new Int16Array(input.length);
  for (let i = 0; i < input.length; i++) {
    const s = Math.max(-1, Math.min(1, input[i]));
    output[i] = s < 0 ? s * 0x8000 : s * 0x7FFF;
  }
  return output;
}

function downsampleBuffer(buffer, inputRate, outputRate = 16000) {
  if (inputRate === outputRate) return buffer;
  const sampleRateRatio = inputRate / outputRate;
  const newLength = Math.round(buffer.length / sampleRateRatio);
  const result = new Float32Array(newLength);
  let offsetResult = 0;
  let offsetBuffer = 0;
  while (offsetResult < result.length) {
    const nextOffsetBuffer = Math.round((offsetResult + 1) * sampleRateRatio);
    let accum = 0, count = 0;
    for (let i = offsetBuffer; i < nextOffsetBuffer && i < buffer.length; i++) {
      accum += buffer[i];
      count++;
    }
    result[offsetResult] = count > 0 ? accum / count : buffer[offsetBuffer];
    offsetResult++;
    offsetBuffer = nextOffsetBuffer;
  }
  return result;
}

function sendFloat32ChunkAsPCM(floatData) {
  const pcm16 = floatTo16BitPCM(floatData);
  const bytes = new Uint8Array(pcm16.buffer);
  let binary = '';
  const chunkSize = 0x4000;
  for (let i = 0; i < bytes.length; i += chunkSize) {
    binary += String.fromCharCode.apply(null, bytes.subarray(i, i + chunkSize));
  }
  sendWS({
    type: 'live_pcm_chunk',
    audio_base64: btoa(binary),
  });
  state.audioChunksSent++;
}

function setupLivePCMStreaming() {
  if (state.liveProcessor) return;
  if (!state.audioCtx || !state.micSource) return;

  // Reset audio token savings counters
  state.audioChunksSent = 0;
  state.audioChunksGated = 0;
  updateSavingsUI();

  // Initial 2.5s greeting lock: bot delivers opening speech cleanly without mic rustle
  state.isInitialGreetingLock = true;
  if (state.greetingLockTimeout) clearTimeout(state.greetingLockTimeout);
  state.greetingLockTimeout = setTimeout(() => {
    state.isInitialGreetingLock = false;
    console.log('🔓 2.5s initial greeting lock released — direct duplex audio open');
  }, 2500);

  const bufferSize = 2048;
  const processor = state.audioCtx.createScriptProcessor(bufferSize, 1, 1);

  // Mute gain node to prevent mic loopback into local speakers
  const muteGain = state.audioCtx.createGain();
  muteGain.gain.value = 0.0;

  let noiseFloor = 0.0025;
  let silenceFrames = 0;
  let isGated = false;
  let trailingSilenceSent = 0;
  const preBuffer = []; // 12 chunks (~480ms pre-buffer so leading consonants/syllables are never clipped)
  const MAX_PREBUFFER = 12;
  const SILENCE_HOLDOVER_FRAMES = 24; // ~1000ms holdover to support natural conversational pauses between words

  processor.onaudioprocess = e => {
    if (state.callStatus !== 'active' || state.isMuted) return;

    const rawInput = e.inputBuffer.getChannelData(0);

    // Clean software digital preamp (1.65x = ~+4.3dB boost) to elevate standard laptop/Mac mic levels
    const boostedData = new Float32Array(rawInput.length);
    let sum = 0;
    for (let i = 0; i < rawInput.length; i++) {
      const sample = Math.max(-1, Math.min(1, rawInput[i] * 1.65));
      boostedData[i] = sample;
      sum += sample * sample;
    }
    const rms = Math.sqrt(sum / rawInput.length);

    // Check if bot is physically outputting sound right now
    const isBotAudioPlaying = state.isBotSpeaking && livePlaybackCtx && (livePlaybackCtx.currentTime < liveNextPlaybackTime - 0.05);

    // Dynamic background noise floor tracking when nobody is speaking
    if (!state.isSpeaking && !isBotAudioPlaying && rms < 0.008) {
      noiseFloor = noiseFloor * 0.95 + rms * 0.05;
    }

    // Highly responsive conversational threshold (picks up natural speech effortlessly at 0.0050)
    const vadThreshold = Math.max(0.0050, noiseFloor * 1.35);

    // Downsample boosted input to 16,000Hz PCM
    const downsampled = downsampleBuffer(boostedData, state.audioCtx.sampleRate, 16000);

    // ── 0. Initial 2.5s Greeting Lock: Hold mic in pre-buffer while bot speaks opening ──
    if (state.isInitialGreetingLock) {
      preBuffer.push(new Float32Array(downsampled));
      if (preBuffer.length > MAX_PREBUFFER) preBuffer.shift();
      return;
    }

    // ── 1. While Bot is Speaking: Prevent Echo Leak, Allow Intentional Barge-In ──
    if (isBotAudioPlaying) {
      // If user speaks deliberately loud over bot audio to interrupt:
      if (rms >= 0.024) {
        console.log('⚡ User barge-in detected (RMS:', rms.toFixed(4), ') — interrupting bot');
        stopLivePlaybackImmediately();
      } else {
        // Suppress speaker feedback leak from being sent to Gemini Live
        preBuffer.push(new Float32Array(downsampled));
        if (preBuffer.length > MAX_PREBUFFER) preBuffer.shift();
        return;
      }
    }

    // ── 2. Normal Conversation / Active Listening Mode ──
    const isUserSpeaking = rms >= vadThreshold;

    if (isUserSpeaking) {
      silenceFrames = 0;
      trailingSilenceSent = 0;
      state.isSpeaking = true;
      state.isBotSpeaking = false;

      // Update UI immediately on very first speech frame!
      if (currentBotTurn) {
        const botSpan = currentBotTurn.querySelector('.turn-text');
        if (botSpan) botSpan.classList.remove('is-streaming');
        currentBotTurn.dataset.ended = 'true';
        currentBotTurn = null;
      }
      setOrbState('YOU', 'SPEAKING');
      activatePipelineStep('mic');

      // Flush pre-buffer when transitioning from silence so initial syllables ("Sixty", "I'm", "Yes") are 100% preserved
      if (isGated) {
        while (preBuffer.length > 0) {
          sendFloat32ChunkAsPCM(preBuffer.shift());
        }
        isGated = false;
      }

      sendFloat32ChunkAsPCM(downsampled);
    } else {
      silenceFrames++;

      // Natural speech hangover: keep streaming for ~1.0s through normal pauses between words
      if (silenceFrames <= SILENCE_HOLDOVER_FRAMES) {
        state.isSpeaking = true;
        sendFloat32ChunkAsPCM(downsampled);
      } else {
        // True silence detected: gate stream to save tokens
        state.isSpeaking = false;
        isGated = true;
        state.audioChunksGated++;

        // Send 6 frames (~250ms) of low trailing silence so Gemini Live's server VAD detects turn boundary cleanly
        if (trailingSilenceSent < 6) {
          trailingSilenceSent++;
          sendFloat32ChunkAsPCM(downsampled);
        }

        // Maintain rolling pre-buffer
        preBuffer.push(new Float32Array(downsampled));
        if (preBuffer.length > MAX_PREBUFFER) preBuffer.shift();

        // Return orb to LISTENING if bot has not started speaking
        if (!state.isBotSpeaking && state.callStatus === 'active') {
          const agentName = (state.agentName || 'AGENT').toUpperCase();
          setOrbState(agentName, 'LISTENING...');
        }
      }
    }

    // Refresh token savings telemetry every 10 frames (~400ms)
    if ((state.audioChunksSent + state.audioChunksGated) % 10 === 0) {
      updateSavingsUI();
    }
  };

  state.micSource.connect(processor);
  processor.connect(muteGain);
  muteGain.connect(state.audioCtx.destination);
  state.liveProcessor = processor;
}

function teardownLivePCMStreaming() {
  if (state.liveProcessor) {
    try { state.liveProcessor.disconnect(); } catch (_) {}
    state.liveProcessor = null;
  }
}

let livePlaybackCtx = null;
let liveNextPlaybackTime = 0;
const liveActiveSources = [];
let botSpeakingEndTimeout = null;

function scheduleCheckBotSpeechEnd() {
  if (botSpeakingEndTimeout) {
    clearTimeout(botSpeakingEndTimeout);
  }

  const ctx = state.audioCtx || livePlaybackCtx;
  const now = ctx ? ctx.currentTime : 0;
  const remainingSec = Math.max(0, liveNextPlaybackTime - now);
  const delayMs = Math.max(60, Math.round(remainingSec * 1000) + 40);

  botSpeakingEndTimeout = setTimeout(() => {
    const currentNow = ctx ? ctx.currentTime : 0;
    if (currentNow >= liveNextPlaybackTime - 0.05) {
      liveActiveSources.length = 0;
      state.isBotSpeaking = false;
      state.isSpeaking = false;
      if (state.callStatus === 'active') {
        if (state.isTerminalCall) {
          console.log('🏁 Terminal disposition reached and audio finished playing — concluding call with', state.lastDisposition);
          setTimeout(() => {
            if (state.callStatus === 'active') {
              sendWS({ type: 'manual_hangup', disposition: state.lastDisposition || 'XFER' });
            }
          }, 350);
        } else {
          const agentName = (state.agentName || 'AGENT').toUpperCase();
          setOrbState(agentName, 'LISTENING...');
          activatePipelineStep('mic');
        }
      }
    }
  }, delayMs);
}

function playLivePCM24k(base64Audio) {
  if (!base64Audio) return;
  const binary = atob(base64Audio);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  const int16 = new Int16Array(bytes.buffer);
  const float32 = new Float32Array(int16.length);
  for (let i = 0; i < int16.length; i++) {
    float32[i] = int16[i] / 32768.0;
  }

  // Use unified audio context so botAnalyser and destination are in the same context
  if (!state.audioCtx) {
    state.audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  }
  if (state.audioCtx.state === 'suspended') {
    state.audioCtx.resume();
  }
  livePlaybackCtx = state.audioCtx;

  const audioBuffer = state.audioCtx.createBuffer(1, float32.length, 24000);
  audioBuffer.copyToChannel(float32, 0);

  const source = state.audioCtx.createBufferSource();
  source.buffer = audioBuffer;
  source.connect(state.audioCtx.destination);

  if (state.botAnalyser) {
    try { source.connect(state.botAnalyser); } catch (_) {}
  }

  const now = state.audioCtx.currentTime;
  if (liveNextPlaybackTime < now) {
    liveNextPlaybackTime = now;
  }
  source.start(liveNextPlaybackTime);
  liveNextPlaybackTime += audioBuffer.duration;
  console.log('🔊 Playing 24kHz audio chunk, length:', float32.length, 'samples (~' + audioBuffer.duration.toFixed(2) + 's)');

  // Clear any speech-end timer since more audio is actively scheduled
  if (botSpeakingEndTimeout) {
    clearTimeout(botSpeakingEndTimeout);
    botSpeakingEndTimeout = null;
  }

  state.isBotSpeaking = true;
  state.isSpeaking = false;
  const agentName = (state.agentName || 'AGENT').toUpperCase();
  setOrbState(agentName, 'SPEAKING');
  activatePipelineStep('spk');

  liveActiveSources.push(source);
  source.onended = () => {
    const idx = liveActiveSources.indexOf(source);
    if (idx !== -1) liveActiveSources.splice(idx, 1);
    scheduleCheckBotSpeechEnd();
  };
}

function stopLivePlaybackImmediately() {
  if (botSpeakingEndTimeout) {
    clearTimeout(botSpeakingEndTimeout);
    botSpeakingEndTimeout = null;
  }
  for (const src of liveActiveSources) {
    try { src.stop(); } catch (_) {}
  }
  liveActiveSources.length = 0;
  const ctx = state.audioCtx || livePlaybackCtx;
  if (ctx) {
    liveNextPlaybackTime = ctx.currentTime;
  }
  state.isBotSpeaking = false;
  state.isSpeaking = true;
  state.interruptionShockwave = 1.0;
  if (el.interruptionFlash) {
    el.interruptionFlash.classList.remove('hidden');
    setTimeout(() => el.interruptionFlash.classList.add('hidden'), 1500);
  }
  setOrbState('YOU', 'SPEAKING');
  activatePipelineStep('mic');
  sendWS({ type: 'interrupt' });
}

let currentBotTurn = null;
let currentCustomerTurn = null;
let subtitleFadeTimeout = null;

function updateOrbSubtitle(role, text) {
  if (!el.orbLiveSubtitle || !el.subContent) return;
  if (subtitleFadeTimeout) {
    clearTimeout(subtitleFadeTimeout);
    subtitleFadeTimeout = null;
  }

  const isBot = (role === 'bot' || role === 'model');
  if (isBot) {
    el.orbLiveSubtitle.classList.remove('user-speaking');
    if (el.subSpeakerName) el.subSpeakerName.textContent = (state.agentName || 'AGENT').toUpperCase();
  } else {
    el.orbLiveSubtitle.classList.add('user-speaking');
    if (el.subSpeakerName) el.subSpeakerName.textContent = 'YOU';
  }

  if (el.orbLiveSubtitle.classList.contains('hidden') || el.orbLiveSubtitle.dataset.currentRole !== role) {
    el.subContent.textContent = text;
    el.orbLiveSubtitle.dataset.currentRole = role;
  } else {
    el.subContent.textContent += text;
  }

  el.orbLiveSubtitle.classList.remove('hidden', 'fading');

  // If customer is speaking, automatically fade out 750ms after speech ceases
  if (!isBot) {
    scheduleSubtitleFade(750);
  }
}

function scheduleSubtitleFade(delayMs = 400) {
  if (subtitleFadeTimeout) clearTimeout(subtitleFadeTimeout);
  subtitleFadeTimeout = setTimeout(() => {
    if (el.orbLiveSubtitle && !el.orbLiveSubtitle.classList.contains('hidden')) {
      el.orbLiveSubtitle.classList.add('fading');
      setTimeout(() => {
        if (el.orbLiveSubtitle && el.orbLiveSubtitle.classList.contains('fading')) {
          el.orbLiveSubtitle.classList.add('hidden');
          el.orbLiveSubtitle.classList.remove('fading');
          if (el.subContent) el.subContent.textContent = '';
        }
      }, 200);
    }
  }, delayMs);
}

function hideOrbSubtitle() {
  if (subtitleFadeTimeout) {
    clearTimeout(subtitleFadeTimeout);
    subtitleFadeTimeout = null;
  }
  if (el.orbLiveSubtitle) {
    el.orbLiveSubtitle.classList.add('hidden');
    el.orbLiveSubtitle.classList.remove('fading');
    if (el.subContent) el.subContent.textContent = '';
  }
}

function handleLiveTranscript(role, text) {
  if (!text) return;
  const isBot = (role === 'bot' || role === 'model');

  // Add tokens for streamed text turn
  const words = text.trim().split(/\s+/).filter(Boolean).length;
  if (words > 0) {
    recordCallTokens(Math.max(1, Math.round(words * 1.3)));
  }

  // Update floating live subtitle under central circular visualizer
  updateOrbSubtitle(role, text);

  if (isBot) {
    if (currentCustomerTurn) {
      const custSpan = currentCustomerTurn.querySelector('.turn-text');
      if (custSpan) custSpan.classList.remove('is-streaming');
      currentCustomerTurn.dataset.ended = 'true';
      currentCustomerTurn = null;
    }
    if (!currentBotTurn || currentBotTurn.dataset.ended === 'true') {
      currentBotTurn = addTranscriptTurn('bot', text);
      const span = currentBotTurn ? currentBotTurn.querySelector('.turn-text') : null;
      if (span) span.classList.add('is-streaming');
    } else {
      const span = currentBotTurn.querySelector('.turn-text');
      if (span) {
        span.textContent += text;
        span.classList.add('is-streaming');
      }
      el.transcriptBox.scrollTop = el.transcriptBox.scrollHeight;
    }

    if (currentBotTurn) {
      const fullBotText = (currentBotTurn.querySelector('.turn-text')?.textContent || '').toLowerCase();
      if ((fullBotText.includes('not for you') || fullBotText.includes('specifically designed for seniors') || fullBotText.includes('specifically for ages 50')) && !state.isTerminalCall) {
        const allBubbles = Array.from(document.querySelectorAll('.chat-bubble')).map(b => b.textContent).join(' ');
        let dispo = 'UNDRAG';
        if (/\b(8[1-9]|9[0-9]|10[0-9])\b/.test(allBubbles)) {
          dispo = 'OVERAG';
        }
        handleDispositionUpdate(dispo, 'Prospect disqualified: age outside 50-80 group');
      }
    }
  } else {
    if (currentBotTurn) {
      const botSpan = currentBotTurn.querySelector('.turn-text');
      if (botSpan) botSpan.classList.remove('is-streaming');
      currentBotTurn.dataset.ended = 'true';
      currentBotTurn = null;
    }
    if (!currentCustomerTurn || currentCustomerTurn.dataset.ended === 'true') {
      currentCustomerTurn = addTranscriptTurn('customer', text);
      const span = currentCustomerTurn ? currentCustomerTurn.querySelector('.turn-text') : null;
      if (span) span.classList.add('is-streaming');
    } else {
      const span = currentCustomerTurn.querySelector('.turn-text');
      if (span) {
        span.textContent += text;
        span.classList.add('is-streaming');
      }
      el.transcriptBox.scrollTop = el.transcriptBox.scrollHeight;
    }
  }
}

function handleDispositionUpdate(status, notes) {
  if (el.liveDispoVal) {
    el.liveDispoVal.textContent = status;
  }
  if (el.liveDispoPill) {
    el.liveDispoPill.classList.remove('hidden');
  }
  addTranscriptTurn('system', `🎯 VICIdial Status: ${status} (${notes || 'Outcome determined'})`);
  state.lastDisposition = status;
  const terminalCodes = ['XFER', 'NI', 'DNC', 'A', 'DEC', 'UNDRAG', 'OVERAG', 'DNQ'];
  if (terminalCodes.includes(status)) {
    state.isTerminalCall = true;
  }
}

// ---------------------------------------------------------------------------
// WebSocket Message Dispatcher
// ---------------------------------------------------------------------------
async function handleWSMessage(data) {
  switch (data.type) {

    case 'call_started':
      state.callId      = data.call_id;
      state.phoneNumber = data.phone_number;
      state.engine      = data.engine || state.engine;
      startCallTimer();

      if (data.active_key) {
        state.activeKeyIndex = data.active_key;
        document.querySelectorAll('.key-node').forEach(n => n.classList.remove('in-call'));
        const activeNode = document.getElementById(`key-node-${data.active_key}`);
        if (activeNode) activeNode.classList.add('in-call');
        if (el.activeCallKeyBadge) {
          el.activeCallKeyBadge.textContent = `KEY #${data.active_key}`;
          el.activeCallKeyBadge.classList.remove('hidden');
        }
      }
      if (data.key_pool) {
        updateKeyPoolUI(data.key_pool);
      }
      if (data.agent_name) {
        state.agentName = data.agent_name;
      }

      if (data.engine === 'gemini') {
        activatePipelineStep('gemini');
        setOrbState(state.agentName.toUpperCase(), 'SPEAKING');
        setupLivePCMStreaming();
        // Fallback: If after 3.5s no live transcript has appeared, render opening
        const opening = `Hi, this is ${state.agentName}. I’m calling about new benefit options for your age group to see if you qualify. Can I ask how old you are?`;
        setTimeout(() => {
          if (state.callStatus === 'active' && !currentBotTurn && document.querySelectorAll('#transcript-container .chat-bubble').length === 0) {
            addTranscriptTurn('bot', opening);
          }
        }, 3500);
      } else {
        addTranscriptTurn('bot', data.text);
        activatePipelineStep('audio');
        setOrbState(state.agentName.toUpperCase(), 'SPEAKING');
        await speakBot(data.text, data.audio_base64);
        if (state.callStatus === 'active') {
          activatePipelineStep('vad');
          setOrbState(state.agentName.toUpperCase(), 'LISTENING...');
        }
      }
      break;

    // ── Gemini Live Specific Events ──
    case 'live_audio':
      if (data.audio_pcm24k) {
        playLivePCM24k(data.audio_pcm24k);
      }
      break;

    case 'live_transcript':
      handleLiveTranscript(data.role, data.text);
      break;

    case 'live_interrupt':
      if (currentBotTurn) {
        const botSpan = currentBotTurn.querySelector('.turn-text');
        if (botSpan) botSpan.classList.remove('is-streaming');
        currentBotTurn.dataset.ended = 'true';
        currentBotTurn = null;
      }
      scheduleSubtitleFade(200);
      stopLivePlaybackImmediately();
      break;

    case 'live_turn_complete':
      if (currentBotTurn) {
        const botSpan = currentBotTurn.querySelector('.turn-text');
        if (botSpan) botSpan.classList.remove('is-streaming');
        currentBotTurn.dataset.ended = 'true';
        currentBotTurn = null;
      }
      if (currentCustomerTurn) {
        const custSpan = currentCustomerTurn.querySelector('.turn-text');
        if (custSpan) custSpan.classList.remove('is-streaming');
        currentCustomerTurn.dataset.ended = 'true';
        currentCustomerTurn = null;
      }
      scheduleSubtitleFade(800);
      // Wait for all scheduled audio chunks to physically finish playing before switching to LISTENING
      scheduleCheckBotSpeechEnd();
      break;

    case 'key_pool_update':
      if (data.key_pool) {
        updateKeyPoolUI(data.key_pool);
      }
      break;

    case 'disposition_update':
      handleDispositionUpdate(data.status, data.notes);
      break;

    // ── Groq Turn-Based Events ──
    case 'bot_text':
      updateLatencyMetrics(data.metrics);
      if (data.llm_ms && el.metricLlm) el.metricLlm.textContent = `${data.llm_ms}ms`;
      addTranscriptTurn('bot', data.text);
      activatePipelineStep('audio');
      setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'SPEAKING');
      await speakBot(data.text, '');
      if (state.callStatus === 'active') {
        activatePipelineStep('vad');
        setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'LISTENING...');
      }
      break;

    case 'bot_audio':
      if (data.tts_ms && el.metricTts) el.metricTts.textContent = `${data.tts_ms}ms`;
      if (data.audio_base64 && data.audio_base64.length > 50) {
        if (state.isBotSpeaking) {
          stopCurrentPlayback();
          await playWavAudio(data.audio_base64);
          if (state.callStatus === 'active') {
            activatePipelineStep('vad');
            setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'LISTENING...');
          }
        }
      }
      break;

    case 'transcript':
      addTranscriptTurn(data.role, data.text);
      if (data.role === 'customer') {
        activatePipelineStep('llm');
        if (data.stt_ms && el.metricStt) el.metricStt.textContent = `${data.stt_ms}ms`;
      }
      break;

    case 'status':
      if (data.step) activatePipelineStep(data.step);
      const curAgent = (state.agentName || 'AGENT').toUpperCase();
      if (data.state === 'thinking') {
        setOrbState(curAgent, 'THINKING...');
      } else if (data.state === 'speaking') {
        setOrbState(curAgent, 'SPEAKING');
      } else if (data.state === 'listening') {
        setOrbState(curAgent, 'LISTENING...');
      }
      break;

    case 'bot_turn':
      updateLatencyMetrics(data.metrics);
      if (data.reply_text) addTranscriptTurn('bot', data.reply_text);
      activatePipelineStep('audio');
      setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'SPEAKING');
      await speakBot(data.reply_text, data.audio_base64);
      if (state.callStatus === 'active') {
        activatePipelineStep('vad');
        setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'LISTENING...');
      }
      break;

    case 'call_ended':
      state.callStatus     = 'ended';
      state.lastDisposition = data.disposition;
      stopCallTimer();
      updateLatencyMetrics(data.metrics);
      teardownAudioPipeline();
      stopCurrentPlayback();
      hideOrbSubtitle();

      document.querySelectorAll('.key-node').forEach(n => n.classList.remove('in-call'));
      if (el.activeCallKeyBadge) el.activeCallKeyBadge.classList.add('hidden');

      el.btnMute.disabled   = true;
      el.btnHangup.disabled = true;
      el.btnStart.disabled  = false;
      el.btnMute.classList.remove('muted');
      const micOnSvg = el.btnMute.querySelector('.icon-mic');
      const micOffSvg = el.btnMute.querySelector('.icon-mic-off');
      if (micOnSvg) micOnSvg.classList.remove('hidden');
      if (micOffSvg) micOffSvg.classList.add('hidden');
      if (el.muteCaption) el.muteCaption.textContent = 'Mute';
      if (el.startCaption) el.startCaption.textContent = 'Start Call';
      el.agentStatusText.textContent = `Ended (${data.disposition})`;

      if (data.reply_text) {
        activatePipelineStep('audio');
        setOrbState(state.agentName ? state.agentName.toUpperCase() : 'AGENT', 'CLOSING');
        await speakBot(data.reply_text, data.audio_base64);
      }
      setOrbState('CALL', 'ENDED');
      break;

    case 'pong':
      if (el.metricPing) {
        el.metricPing.textContent = `⚡ ${Date.now() - state.lastPingSentTime}ms`;
      }
      break;

    case 'interrupted':
      console.log('✅ Interrupt acknowledged');
      break;

    case 'error':
      console.error('Server error:', data.error);
      el.agentStatusText.textContent = `Error: ${data.error}`;
      setOrbState('ERROR', 'FAILED');
      break;
  }
}

// ---------------------------------------------------------------------------
// speakBot — INSTANT browser TTS, upgrades to WAV if provided
// ---------------------------------------------------------------------------
async function speakBot(text, base64Wav) {
  stopCurrentPlayback();
  state.isBotSpeaking   = true;
  state.botSpeakingText = text || '';

  // If we already have WAV, play it directly (no browser TTS needed)
  if (base64Wav && base64Wav.length > 50) {
    return playWavAudio(base64Wav);
  }

  // Otherwise: browser TTS fires IMMEDIATELY (zero latency)
  return speakBrowserTTS(text);
}

// ---------------------------------------------------------------------------
// WAV audio playback
// ---------------------------------------------------------------------------
function playWavAudio(base64Wav) {
  return new Promise(resolve => {
    try {
      const byteChars = atob(base64Wav);
      const bytes = new Uint8Array(byteChars.length);
      for (let i = 0; i < byteChars.length; i++) bytes[i] = byteChars.charCodeAt(i);
      const blob = new Blob([bytes], { type: 'audio/wav' });
      const url  = URL.createObjectURL(blob);
      const audio = new Audio(url);
      state.currentAudio = audio;

      if (state.audioCtx && state.botAnalyser) {
        try {
          const src = state.audioCtx.createMediaElementSource(audio);
          src.connect(state.botAnalyser);
        } catch (_) {}
      }

      audio.onended = () => {
        URL.revokeObjectURL(url);
        state.isBotSpeaking    = false;
        state.currentAudio    = null;
        state.botSpeakingText = '';
        state.echoLockoutUntil = Date.now() + 700; // 700ms echo lockout after WAV
        state.noiseFloor      = 0.018;
        resolve();
      };
      audio.onerror = () => {
        URL.revokeObjectURL(url);
        state.isBotSpeaking   = false;
        state.currentAudio    = null;
        state.echoLockoutUntil = Date.now() + 500;
        resolve();
      };

      audio.play().catch(() => {
        // Browser blocked autoplay — fallback to browser TTS
        state.currentAudio = null;
        speakBrowserTTS(state.botSpeakingText).then(resolve);
      });
    } catch (err) {
      console.error('WAV playback error:', err);
      state.isBotSpeaking = false;
      resolve();
    }
  });
}

// ---------------------------------------------------------------------------
// Browser TTS (SpeechSynthesis) — primary voice engine
// ---------------------------------------------------------------------------
let _cachedVoices = [];
if (typeof window !== 'undefined' && 'speechSynthesis' in window) {
  _cachedVoices = window.speechSynthesis.getVoices();
  window.speechSynthesis.onvoiceschanged = () => {
    _cachedVoices = window.speechSynthesis.getVoices();
  };
}

function speakBrowserTTS(text) {
  return new Promise(resolve => {
    if (!('speechSynthesis' in window) || !text || !text.trim()) {
      state.isBotSpeaking    = false;
      state.echoLockoutUntil = Date.now() + 300;
      resolve();
      return;
    }

    try {
      window.speechSynthesis.cancel();
      window.speechSynthesis.resume();

      const utterance    = new SpeechSynthesisUtterance(text.trim());
      utterance.rate     = 1.05;
      utterance.pitch    = 1.0;
      utterance.volume   = 1.0;

      // Pick best English voice
      const voices = _cachedVoices.length > 0 ? _cachedVoices : window.speechSynthesis.getVoices();
      const voice  = voices.find(v => v.lang && v.lang.startsWith('en') && (
        v.name.includes('Samantha') || v.name.includes('Alex') ||
        v.name.includes('Natural')  || v.name.includes('Google US English') ||
        v.name.includes('Daniel')
      )) || voices.find(v => v.lang && v.lang.startsWith('en'));
      if (voice) utterance.voice = voice;

      state.currentUtterance = utterance;

      let finished = false;
      let resumeTimer = null;
      let safetyTimer = null;

      const cleanup = () => {
        if (finished) return;
        finished = true;
        if (resumeTimer) clearInterval(resumeTimer);
        if (safetyTimer) clearTimeout(safetyTimer);
        state.isBotSpeaking    = false;
        state.currentUtterance = null;
        state.botSpeakingText  = '';
        state.echoLockoutUntil = Date.now() + 700; // 700ms echo lockout after TTS
        state.noiseFloor       = 0.018;
        resolve();
      };

      utterance.onstart = () => {
        state.isBotSpeaking = true;
        setOrbState('ALEX', 'SPEAKING');
        activatePipelineStep('audio');
      };
      utterance.onend   = cleanup;
      utterance.onerror = e => { console.warn('SpeechSynthesis error:', e.error); cleanup(); };

      // Chrome bug: speech synthesis pauses after ~15s without this keepalive
      resumeTimer = setInterval(() => {
        if (window.speechSynthesis.speaking && !finished) {
          window.speechSynthesis.pause();
          window.speechSynthesis.resume();
        }
      }, 10000);

      // Safety timeout based on word count (~2.5 words/sec) + 2s buffer
      const wordCount = text.trim().split(/\s+/).length;
      const safetyMs  = Math.max(2000, (wordCount / 2.5) * 1000 + 2000);
      safetyTimer = setTimeout(cleanup, safetyMs);

      window.speechSynthesis.speak(utterance);

    } catch (err) {
      console.error('speakBrowserTTS exception:', err);
      state.isBotSpeaking = false;
      resolve();
    }
  });
}

// ---------------------------------------------------------------------------
// Manual Actions
// ---------------------------------------------------------------------------
function toggleMute() {
  state.isMuted = !state.isMuted;
  if (state.micStream) {
    state.micStream.getAudioTracks().forEach(t => { t.enabled = !state.isMuted; });
  }
  const micOnSvg = el.btnMute.querySelector('.icon-mic');
  const micOffSvg = el.btnMute.querySelector('.icon-mic-off');

  if (state.isMuted) {
    el.btnMute.classList.add('muted');
    if (el.muteCaption) el.muteCaption.textContent = 'Unmute';
    if (micOnSvg) micOnSvg.classList.add('hidden');
    if (micOffSvg) micOffSvg.classList.remove('hidden');
    setOrbState('MIC', 'MUTED');
  } else {
    el.btnMute.classList.remove('muted');
    if (el.muteCaption) el.muteCaption.textContent = 'Mute';
    if (micOnSvg) micOnSvg.classList.remove('hidden');
    if (micOffSvg) micOffSvg.classList.add('hidden');
    setOrbState((state.agentName || 'AGENT').toUpperCase(), 'LISTENING...');
  }
}

function onManualHangup() {
  if (state.callStatus !== 'active') return;
  state.callStatus = 'ended';
  stopCallTimer();
  stopCurrentPlayback();
  teardownAudioPipeline();
  hideOrbSubtitle();

  const micOnSvg = el.btnMute.querySelector('.icon-mic');
  const micOffSvg = el.btnMute.querySelector('.icon-mic-off');
  if (micOnSvg) micOnSvg.classList.remove('hidden');
  if (micOffSvg) micOffSvg.classList.add('hidden');

  document.querySelectorAll('.key-node').forEach(n => n.classList.remove('in-call'));
  if (el.activeCallKeyBadge) el.activeCallKeyBadge.classList.add('hidden');

  el.btnMute.disabled   = true;
  el.btnHangup.disabled = true;
  el.btnStart.disabled  = false;
  el.btnMute.classList.remove('muted');
  if (el.muteCaption) el.muteCaption.textContent = 'Mute';
  if (el.startCaption) el.startCaption.textContent = 'Start Call';
  el.agentStatusText.textContent = 'Ended (Caller Hangup)';
  setOrbState('CALL', 'ENDED');

  sendWS({ type: 'manual_hangup', disposition: 'CH' });
}

// ---------------------------------------------------------------------------
// Transcript
// ---------------------------------------------------------------------------
function addTranscriptTurn(role, text) {
  if (!text) return null;
  if (el.transcriptEmpty) el.transcriptEmpty.style.display = 'none';

  const turn = document.createElement('div');
  const isBot = (role === 'bot' || role === 'model');
  const isSys = (role === 'system');

  turn.className = `chat-bubble ${isBot ? 'bubble-bot' : (isSys ? 'bubble-sys' : 'bubble-user')}`;
  
  const now = new Date();
  const timeStr = now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  const agentDisplayName = state.agentName || 'Agent';
  let speakerLabel = isBot ? `${agentDisplayName} (Agent)` : 'You (Homeowner)';
  if (isSys) speakerLabel = 'System Notification';

  turn.innerHTML = `
    <div class="bubble-speaker">
      <span>${speakerLabel}</span>
      <span class="bubble-time">${timeStr}</span>
    </div>
    <div class="bubble-text"><span class="turn-text">${escHtml(text)}</span></div>
  `;
  el.transcriptBox.appendChild(turn);
  el.transcriptBox.scrollTop = el.transcriptBox.scrollHeight;
  return turn;
}

function clearTranscript() {
  if (currentBotTurn) {
    const botSpan = currentBotTurn.querySelector('.turn-text');
    if (botSpan) botSpan.classList.remove('is-streaming');
    currentBotTurn = null;
  }
  if (currentCustomerTurn) {
    const custSpan = currentCustomerTurn.querySelector('.turn-text');
    if (custSpan) custSpan.classList.remove('is-streaming');
    currentCustomerTurn = null;
  }
  document.querySelectorAll('#transcript-container .chat-bubble').forEach(n => n.remove());
  document.querySelectorAll('#transcript-container .turn').forEach(n => n.remove());
  if (el.transcriptEmpty) el.transcriptEmpty.style.display = '';
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------
function setOrbState(title, sub) {
  if (title === 'ALEX') {
    title = (state.agentName || 'AGENT').toUpperCase();
  }
  if (el.orbStateTitle) {
    el.orbStateTitle.textContent = title;
    if (title === 'YOU') {
      el.orbStateTitle.style.color = '#10b981';
      el.orbStateTitle.style.textShadow = '0 0 16px rgba(16, 185, 129, 0.6)';
    } else {
      el.orbStateTitle.style.color = '#ffffff';
      el.orbStateTitle.style.textShadow = '0 0 14px rgba(0, 240, 255, 0.4)';
    }
  }
  if (el.orbStateSub) {
    el.orbStateSub.textContent = sub;
    if (title === 'YOU') {
      el.orbStateSub.style.color = '#6ee7b7';
    } else if (sub.includes('SPEAKING')) {
      el.orbStateSub.style.color = 'var(--cyan)';
    } else {
      el.orbStateSub.style.color = 'var(--text-muted)';
    }
  }
  if (el.orbSoundBars) {
    el.orbSoundBars.classList.toggle('speaking', sub.includes('SPEAKING'));
    el.orbSoundBars.querySelectorAll('.bar').forEach(b => {
      b.style.background = (title === 'YOU') ? '#10b981' : 'var(--cyan)';
      b.style.boxShadow = (title === 'YOU') ? '0 0 8px #10b981' : '0 0 6px var(--cyan)';
    });
  }
}

function escHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function getSupportedMimeType() {
  const types = [
    'audio/webm;codecs=opus',
    'audio/webm',
    'audio/ogg;codecs=opus',
    'audio/ogg',
    'audio/mp4',
  ];
  for (const t of types) {
    if (typeof MediaRecorder !== 'undefined' && MediaRecorder.isTypeSupported(t)) return t;
  }
  return '';
}

// Ensure mic hardware access is unconditionally terminated if tab closes or reloads
window.addEventListener('beforeunload', () => {
  teardownAudioPipeline();
});
window.addEventListener('pagehide', () => {
  teardownAudioPipeline();
});
