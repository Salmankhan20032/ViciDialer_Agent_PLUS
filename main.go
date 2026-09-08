package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Call Session
// ---------------------------------------------------------------------------

// CallSession holds the state for a single active or historical call.
type CallSession struct {
	ID          string        `json:"id"`
	PhoneNumber string        `json:"phone_number"`
	StartedAt   time.Time     `json:"started_at"`
	EndedAt     *time.Time    `json:"ended_at,omitempty"`
	Status      string        `json:"status"` // "active" | "ended"
	Disposition *string       `json:"disposition,omitempty"`
	History     []ChatMessage `json:"-"` // full conv history incl. system prompt
	Transcript  []TurnEntry   `json:"transcript"`
}

// TurnEntry is one exchange stored for the history panel and call log.
type TurnEntry struct {
	Role      string    `json:"role"` // "customer" | "bot"
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp"`
}

// ---------------------------------------------------------------------------
// Server
// ---------------------------------------------------------------------------

// Server wires together all components and owns the in-memory session store.
type Server struct {
	groq            *GroqClient
	mu              sync.Mutex
	sessions        map[string]*CallSession
	history         []*CallSession // ordered list for /api/call/history
	sysPrompt       string
	geminiSysPrompt string
	voiceEngine     string
	geminiApiKey    string
	geminiKeyPool   *KeyPool
	geminiVoice     string
	geminiModel     string
}

func NewServer(groq *GroqClient) *Server {
	return &Server{
		groq:            groq,
		sessions:        make(map[string]*CallSession),
		sysPrompt:       BuildSystemPrompt(DefaultCampaignScript),
		geminiSysPrompt: BuildGeminiLiveSystemPrompt(DefaultCampaignScript, "Sarah"),
		voiceEngine:     "gemini",
		geminiVoice:     "Aoede",
		geminiModel:     "models/gemini-2.5-flash-native-audio-latest",
	}
}

// ---------------------------------------------------------------------------
// Route handlers
// ---------------------------------------------------------------------------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Prevent aggressive browser caching during development/simulator use
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Simple router — stdlib only.
	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/":
		http.ServeFile(w, r, "static/index.html")

	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && strings.HasPrefix(r.URL.Path, "/static/"):
		http.StripPrefix("/static/", http.FileServer(http.Dir("static"))).ServeHTTP(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/api/dispositions":
		s.handleDispositions(w, r)

	case r.Method == http.MethodPost && r.URL.Path == "/api/call/start":
		s.handleCallStart(w, r)

	case r.Method == http.MethodPost && r.URL.Path == "/api/call/turn":
		s.handleCallTurn(w, r)

	case r.Method == http.MethodPost && r.URL.Path == "/api/call/hangup":
		s.handleCallHangup(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/api/call/history":
		s.handleCallHistory(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/api/config":
		s.handleConfig(w, r)

	case r.Method == http.MethodGet && r.URL.Path == "/api/key_pool":
		s.handleKeyPool(w, r)

	case r.URL.Path == "/ws/call":
		s.handleWebSocket(w, r)

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleKeyPool(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.geminiKeyPool == nil {
		http.Error(w, `{"error":"no key pool configured"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(s.geminiKeyPool.Stats())
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var poolStats map[string]interface{}
	hasGemini := s.geminiApiKey != ""
	if s.geminiKeyPool != nil {
		poolStats = s.geminiKeyPool.Stats()
		if s.geminiKeyPool.HasKeys() {
			hasGemini = true
		}
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"voice_engine":   s.voiceEngine,
		"has_gemini_key": hasGemini,
		"has_groq_key":   s.groq != nil,
		"gemini_voice":   s.geminiVoice,
		"gemini_model":   s.geminiModel,
		"key_pool":       poolStats,
	})
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // local simulator
	},
}

// handleWebSocket manages full-duplex live voice streaming between browser and Groq.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	defer conn.Close()

	var writeMu sync.Mutex
	safeSend := func(msg interface{}) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteJSON(msg)
	}

	var (
		sess            *CallSession
		turnCancel      context.CancelFunc
		turnMu          sync.Mutex
		geminiLive      *GeminiLiveSession
		userAudioChunks int
	)

	cancelActiveTurn := func() {
		turnMu.Lock()
		defer turnMu.Unlock()
		if turnCancel != nil {
			turnCancel()
			turnCancel = nil
		}
	}

	defer func() {
		cancelActiveTurn()
		if geminiLive != nil {
			geminiLive.Close()
			geminiLive = nil
		}
	}()

	for {
		var req struct {
			Type        string `json:"type"`
			AudioBase64 string `json:"audio_base64"`
			Format      string `json:"format"`
			Disposition string `json:"disposition"`
			Engine      string `json:"engine"`
			Voice       string `json:"voice"`
		}
		if err := conn.ReadJSON(&req); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WS connection closed: %v", err)
			}
			cancelActiveTurn()
			if geminiLive != nil {
				geminiLive.Close()
				geminiLive = nil
			}
			break
		}

		switch req.Type {
		case "start_call":
			cancelActiveTurn()
			if geminiLive != nil {
				geminiLive.Close()
				geminiLive = nil
			}

			engine := req.Engine
			if engine == "" {
				engine = s.voiceEngine
			}

			if engine == "gemini" {
				if s.geminiKeyPool == nil || !s.geminiKeyPool.HasKeys() {
					_ = safeSend(map[string]interface{}{
						"type":  "error",
						"error": "No Gemini API keys configured in .env! Add GEMINI_API_KEYS=key1,key2... to .env and restart.",
					})
					continue
				}

				voice := req.Voice
				if voice == "" {
					voice = s.geminiVoice
				}

				agentName, agentGender := PickAgentName(voice)

				sess = s.newSession()
				s.mu.Lock()
				s.sessions[sess.ID] = sess
				s.mu.Unlock()

				sysPrompt := BuildGeminiLiveSystemPrompt(DefaultCampaignScript, agentName)

				var gSession *GeminiLiveSession
				var chosenKey string
				var chosenKeyIdx int
				var connectErr error

				poolSize := s.geminiKeyPool.Size()
				if poolSize == 0 {
					poolSize = 1
				}

				// Attempt connection with healthy key from pool, auto-failing over on rate limit
				for attempt := 0; attempt < poolSize; attempt++ {
					k, idx, err := s.geminiKeyPool.NextKey()
					if err != nil {
						connectErr = err
						break
					}
					chosenKey = k
					chosenKeyIdx = idx

					log.Printf("[%s] 🔑 Connecting to Gemini Live using Key #%d (%s) [Attempt %d/%d]",
						sess.ID, chosenKeyIdx+1, MaskKey(chosenKey), attempt+1, poolSize)

					candSession := NewGeminiLiveSession(chosenKey, s.geminiModel, voice, sysPrompt)

					candSession.OnAudio = func(pcm24kBase64 string) {
						_ = safeSend(map[string]interface{}{
							"type":         "live_audio",
							"audio_pcm24k": pcm24kBase64,
						})
					}

					candSession.OnTranscript = func(role string, text string) {
						if sess != nil && text != "" {
							if len(sess.Transcript) > 0 && sess.Transcript[len(sess.Transcript)-1].Role == role {
								sess.Transcript[len(sess.Transcript)-1].Text += text
							} else {
								sess.Transcript = append(sess.Transcript, TurnEntry{
									Role: role, Text: text, Timestamp: time.Now(),
								})
							}
						}
						_ = safeSend(map[string]interface{}{
							"type": "live_transcript",
							"role": role,
							"text": text,
						})
					}

					candSession.OnInterrupted = func() {
						log.Printf("[%s] ⚡ Gemini Live barge-in: bot interrupted by user", sess.ID)
						_ = safeSend(map[string]interface{}{
							"type":    "live_interrupt",
							"message": "Interrupted by user speech",
						})
					}

					candSession.OnTurnComplete = func() {
						_ = safeSend(map[string]interface{}{
							"type": "live_turn_complete",
						})
					}

					candSession.OnDisposition = func(code string, notes string) {
						if sess != nil {
							sess.Disposition = &code
						}
						log.Printf("[%s] 🎯 VICIdial Disposition updated: %s (%s)", sess.ID, code, notes)
						_ = safeSend(map[string]interface{}{
							"type":   "disposition_update",
							"status": code,
							"notes":  notes,
						})
					}

					candSession.OnError = func(err error) {
						log.Printf("[%s] Gemini Live error on Key #%d: %v", sess.ID, chosenKeyIdx+1, err)
						errStr := strings.ToLower(err.Error())
						if strings.Contains(errStr, "rate") || strings.Contains(errStr, "429") || strings.Contains(errStr, "quota") {
							s.geminiKeyPool.ReportRateLimit(chosenKey)
							_ = safeSend(map[string]interface{}{
								"type":     "key_pool_update",
								"key_pool": s.geminiKeyPool.Stats(),
							})
						}
						_ = safeSend(map[string]interface{}{
							"type":  "error",
							"error": "Gemini Live error: " + err.Error(),
						})
					}

					connCtx, connCancel := context.WithTimeout(context.Background(), 12*time.Second)
					cErr := candSession.Connect(connCtx)
					connCancel()

					if cErr != nil {
						log.Printf("[%s] Gemini Live connect failed on Key #%d (%s): %v",
							sess.ID, chosenKeyIdx+1, MaskKey(chosenKey), cErr)
						cErrStr := strings.ToLower(cErr.Error())
						if strings.Contains(cErrStr, "suspended") || strings.Contains(cErrStr, "permission denied") || strings.Contains(cErrStr, "invalid") {
							s.geminiKeyPool.ReportPermanentFailure(chosenKey, cErr.Error())
						} else {
							s.geminiKeyPool.ReportRateLimit(chosenKey)
						}
						connectErr = cErr
						candSession.Close()
						continue
					}

					s.geminiKeyPool.ReportSuccess(chosenKey)
					gSession = candSession
					connectErr = nil
					break
				}

				if gSession == nil {
					log.Printf("[%s] Gemini Live all keys failed: %v", sess.ID, connectErr)
					_ = safeSend(map[string]interface{}{
						"type":  "error",
						"error": "Failed to connect to Gemini Live on any key in pool: " + connectErr.Error(),
					})
					continue
				}

				geminiLive = gSession

				_ = safeSend(map[string]interface{}{
					"type":         "call_started",
					"call_id":      sess.ID,
					"phone_number": sess.PhoneNumber,
					"engine":       "gemini",
					"voice":        voice,
					"agent_name":   agentName,
					"agent_gender": agentGender,
					"active_key":   chosenKeyIdx + 1,
					"key_pool":     s.geminiKeyPool.Stats(),
				})

				log.Printf("[%s] 🎙️ Call started with %s (%s, voice: %s)", sess.ID, agentName, agentGender, voice)

				// Trigger opening greeting
				if err := geminiLive.SendOpeningPrompt(agentName); err != nil {
					log.Printf("[%s] Failed to send opening greeting prompt: %v", sess.ID, err)
				}
				continue
			}

			// Groq Fallback Start Call
			if s.groq == nil {
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "Groq client is not initialized and GEMINI_API_KEY is not configured.",
				})
				continue
			}

			sess = s.newSession()

			sess.History = append(sess.History, ChatMessage{
				Role:    "user",
				Content: "[CALL CONNECTED — the prospect just picked up. Deliver your opening line now.]",
			})

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			turnMu.Lock()
			turnCancel = cancel
			turnMu.Unlock()

			decision, err := s.groq.ChatWithContext(ctx, sess.History)
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				log.Printf("[%s] WS start chat error: %v", sess.ID, err)
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "LLM error: " + err.Error(),
				})
				continue
			}

			sess.History = append(sess.History, ChatMessage{
				Role:    "assistant",
				Content: decision.Reply,
			})
			sess.Transcript = append(sess.Transcript, TurnEntry{
				Role: "bot", Text: decision.Reply, Timestamp: time.Now(),
			})

			s.mu.Lock()
			s.sessions[sess.ID] = sess
			s.mu.Unlock()

			// Send text IMMEDIATELY so browser TTS fires with zero network wait
			_ = safeSend(map[string]interface{}{
				"type":         "call_started",
				"call_id":      sess.ID,
				"phone_number": sess.PhoneNumber,
				"text":         decision.Reply,
				"audio_base64": "",
				"engine":       "groq",
			})

			// Fire Groq TTS in background — send bot_audio if it arrives within 3.5s
			go func(reply string, parentCtx context.Context) {
				ttsCtx, ttsCancel := context.WithTimeout(parentCtx, 3500*time.Millisecond)
				defer ttsCancel()
				audioB64, ttsErr := s.groq.SpeakBase64WithContext(ttsCtx, reply)
				if ttsErr != nil || audioB64 == "" {
					return
				}
				_ = safeSend(map[string]interface{}{
					"type":         "bot_audio",
					"audio_base64": audioB64,
				})
			}(decision.Reply, ctx)

		case "live_pcm_chunk":
			if geminiLive != nil && req.AudioBase64 != "" {
				userAudioChunks++
				if userAudioChunks == 1 || userAudioChunks%50 == 0 {
					sessID := "live"
					if sess != nil {
						sessID = sess.ID
					}
					log.Printf("[%s] 🎤 Streaming user audio to Gemini Live (chunk #%d, %d chars)", sessID, userAudioChunks, len(req.AudioBase64))
				}
				if err := geminiLive.SendPCMChunk(req.AudioBase64); err != nil {
					// Ignore transient write errors during teardown
				}
			}

		case "interrupt":
			cancelActiveTurn()
			if sess != nil {
				log.Printf("[%s] User interrupted bot (barge-in)", sess.ID)
			}
			_ = safeSend(map[string]interface{}{
				"type":    "interrupted",
				"message": "Playback interrupted",
			})

		case "audio_turn":
			if sess == nil || sess.Status == "ended" {
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "No active call session",
				})
				continue
			}

			cancelActiveTurn()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			turnMu.Lock()
			turnCancel = cancel
			turnMu.Unlock()

			audioBytes, err := base64.StdEncoding.DecodeString(req.AudioBase64)
			if err != nil {
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "Failed to decode audio: " + err.Error(),
				})
				continue
			}

			ext := req.Format
			if ext == "" {
				ext = "webm"
			}
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}

			tTurnStart := time.Now()

			_ = safeSend(map[string]interface{}{
				"type":    "status",
				"state":   "thinking",
				"message": "Transcribing speech...",
				"step":    "stt",
			})

			tSTTStart := time.Now()
			transcript, err := s.groq.TranscribeWithContext(ctx, audioBytes, "audio"+ext)
			sttMs := time.Since(tSTTStart).Milliseconds()
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				log.Printf("[%s] WS STT error: %v", sess.ID, err)
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "Transcription error: " + err.Error(),
				})
				continue
			}

			cleanTranscript := strings.TrimSpace(transcript)
			lowerTranscript := strings.ToLower(cleanTranscript)

			// 1. Filter empty / punctuation-only / silence markers
			if cleanTranscript == "" || cleanTranscript == "." || cleanTranscript == "..." ||
				strings.HasPrefix(lowerTranscript, "[silence") ||
				strings.HasPrefix(lowerTranscript, "[blank_audio") {
				log.Printf("[%s] Dropping empty / silent customer audio", sess.ID)
				_ = safeSend(map[string]interface{}{
					"type":  "status",
					"state": "listening",
					"step":  "vad",
				})
				continue
			}

			// 2. Filter Whisper ambient-noise hallucinations
			if isWhisperHallucination(cleanTranscript) {
				log.Printf("[%s] Dropped Whisper hallucination: %q", sess.ID, cleanTranscript)
				_ = safeSend(map[string]interface{}{
					"type":  "status",
					"state": "listening",
					"step":  "vad",
				})
				continue
			}

			// 3. Filter acoustic echo (mic picking up bot speaker output)
			var botSpeechList []string
			for i := len(sess.Transcript) - 1; i >= 0; i-- {
				if sess.Transcript[i].Role == "bot" {
					botSpeechList = append(botSpeechList, sess.Transcript[i].Text)
					if len(botSpeechList) >= 3 {
						break
					}
				}
			}
			botSpeechList = append(botSpeechList, DefaultCampaignScript)

			if isAcousticEcho(cleanTranscript, botSpeechList) {
				log.Printf("[%s] Dropped acoustic echo: %q", sess.ID, cleanTranscript)
				_ = safeSend(map[string]interface{}{
					"type":  "status",
					"state": "listening",
					"step":  "vad",
				})
				continue
			}

			log.Printf("[%s] customer: %q (STT: %dms)", sess.ID, cleanTranscript, sttMs)

			// Send customer transcript immediately
			_ = safeSend(map[string]interface{}{
				"type":   "transcript",
				"role":   "customer",
				"text":   cleanTranscript,
				"stt_ms": sttMs,
			})

			sess.History = append(sess.History, ChatMessage{
				Role:    "user",
				Content: cleanTranscript,
			})
			sess.Transcript = append(sess.Transcript, TurnEntry{
				Role: "customer", Text: cleanTranscript, Timestamp: time.Now(),
			})

			_ = safeSend(map[string]interface{}{
				"type":    "status",
				"state":   "thinking",
				"message": "Generating response...",
				"step":    "llm",
			})

			tLLMStart := time.Now()
			decision, err := s.groq.ChatWithContext(ctx, sess.History)
			llmMs := time.Since(tLLMStart).Milliseconds()
			if err != nil {
				if ctx.Err() != nil {
					continue
				}
				log.Printf("[%s] WS chat error: %v", sess.ID, err)
				_ = safeSend(map[string]interface{}{
					"type":  "error",
					"error": "LLM error: " + err.Error(),
				})
				continue
			}

			log.Printf("[%s] bot: status=%s disposition=%v (LLM: %dms) reply=%q",
				sess.ID, decision.Status, decision.Disposition, llmMs, decision.Reply)

			if decision.Reply != "" {
				sess.History = append(sess.History, ChatMessage{
					Role:    "assistant",
					Content: decision.Reply,
				})
				sess.Transcript = append(sess.Transcript, TurnEntry{
					Role: "bot", Text: decision.Reply, Timestamp: time.Now(),
				})
			}

			totalMs := time.Since(tTurnStart).Milliseconds()
			metrics := map[string]interface{}{
				"stt_ms":   sttMs,
				"llm_ms":   llmMs,
				"tts_ms":   0,
				"total_ms": totalMs,
			}

			if decision.Status == "ended" && decision.Disposition != nil {
				cleanDisp := strings.TrimSpace(*decision.Disposition)
				decision.Disposition = &cleanDisp
				s.closeSession(sess, cleanDisp)

				// Send call_ended with text immediately (browser TTS fires)
				_ = safeSend(map[string]interface{}{
					"type":         "call_ended",
					"call_id":      sess.ID,
					"disposition":  cleanDisp,
					"reasoning":    decision.Reasoning,
					"reply_text":   decision.Reply,
					"audio_base64": "", // fire browser TTS; Groq audio sent via bot_audio if arrives in time
					"metrics":      metrics,
				})

				// Groq TTS in background for call_ended too
				if decision.Reply != "" {
					go func(reply string, parentCtx context.Context) {
						ttsCtx, ttsCancel := context.WithTimeout(parentCtx, 3500*time.Millisecond)
						defer ttsCancel()
						audioB64, ttsErr := s.groq.SpeakBase64WithContext(ttsCtx, reply)
						if ttsErr != nil || audioB64 == "" {
							return
						}
						_ = safeSend(map[string]interface{}{
							"type":         "bot_audio",
							"audio_base64": audioB64,
						})
					}(decision.Reply, ctx)
				}

			} else {
				// KEY CHANGE: Send bot_text immediately (triggers browser TTS on client)
				_ = safeSend(map[string]interface{}{
					"type":    "bot_text",
					"text":    decision.Reply,
					"llm_ms": llmMs,
					"metrics": metrics,
					"status":  decision.Status,
				})

				// Then fire Groq TTS async — sends bot_audio if arrives < 3.5s
				if decision.Reply != "" {
					go func(reply string, parentCtx context.Context) {
						ttsCtx, ttsCancel := context.WithTimeout(parentCtx, 3500*time.Millisecond)
						defer ttsCancel()
						tTTSStart := time.Now()
						audioB64, ttsErr := s.groq.SpeakBase64WithContext(ttsCtx, reply)
						ttsMs := time.Since(tTTSStart).Milliseconds()
						if ttsErr != nil || audioB64 == "" {
							return // browser TTS already running; no action needed
						}
						log.Printf("[%s] Groq TTS ready in %dms — sending bot_audio", sess.ID, ttsMs)
						_ = safeSend(map[string]interface{}{
							"type":         "bot_audio",
							"audio_base64": audioB64,
							"tts_ms":       ttsMs,
						})
					}(decision.Reply, ctx)
				}
			}

		case "ping":
			_ = safeSend(map[string]interface{}{
				"type": "pong",
				"time": req.Format,
			})

		case "manual_hangup":
			if sess == nil {
				continue
			}
			cancelActiveTurn()
			code := req.Disposition
			if code == "" {
				code = "CH"
			}
			s.closeSession(sess, code)
			log.Printf("[%s] manual hangup — disposition: %s", sess.ID, code)
			_ = safeSend(map[string]interface{}{
				"type":        "call_ended",
				"call_id":     sess.ID,
				"disposition": code,
				"manual":      true,
			})
		}
	}
}


// GET /api/dispositions — frontend pulls the code list from the same Go source.
func (s *Server) handleDispositions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Dispositions)
}

// POST /api/call/start
func (s *Server) handleCallStart(w http.ResponseWriter, r *http.Request) {
	sess := s.newSession()

	// Ask the LLM for the opening line with a bootstrap user message.
	sess.History = append(sess.History, ChatMessage{
		Role:    "user",
		Content: "[CALL CONNECTED — the prospect just picked up. Deliver your opening line now.]",
	})

	decision, err := s.groq.Chat(sess.History)
	if err != nil {
		log.Printf("call/start chat error: %v", err)
		writeError(w, http.StatusInternalServerError, "LLM error: "+err.Error())
		return
	}

	// Append bot turn to history.
	sess.History = append(sess.History, ChatMessage{
		Role:    "assistant",
		Content: decision.Reply,
	})
	sess.Transcript = append(sess.Transcript, TurnEntry{
		Role: "bot", Text: decision.Reply, Timestamp: time.Now(),
	})

	// Synthesise audio.
	audioB64, err := s.groq.SpeakBase64(decision.Reply)
	if err != nil {
		log.Printf("call/start TTS error: %v", err)
		// Non-fatal — send empty audio, UI will still show the text.
		audioB64 = ""
	}

	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()

	log.Printf("[%s] call started — %s | opening: %q", sess.ID, sess.PhoneNumber, decision.Reply)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"call_id":      sess.ID,
		"phone_number": sess.PhoneNumber,
		"text":         decision.Reply,
		"audio_base64": audioB64,
	})
}

// POST /api/call/turn — supports multipart (call_id + audio file) or JSON (call_id + customer_speech)
func (s *Server) handleCallTurn(w http.ResponseWriter, r *http.Request) {
	var (
		callID     string
		transcript string
	)

	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			CallID         string `json:"call_id"`
			CustomerSpeech string `json:"customer_speech"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
		callID = req.CallID
		transcript = req.CustomerSpeech
	} else {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
			return
		}
		callID = r.FormValue("call_id")
		if callID == "" {
			writeError(w, http.StatusBadRequest, "missing call_id")
			return
		}

		file, header, err := r.FormFile("audio")
		if err == nil {
			defer file.Close()
			audioData, err := io.ReadAll(file)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "read audio: "+err.Error())
				return
			}
			t, err := s.groq.Transcribe(audioData, header.Filename)
			if err != nil {
				log.Printf("[%s] STT error: %v", callID, err)
			} else {
				transcript = t
			}
		} else {
			transcript = r.FormValue("customer_speech")
		}
	}

	if callID == "" {
		writeError(w, http.StatusBadRequest, "missing call_id")
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[callID]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown call_id")
		return
	}
	if sess.Status == "ended" {
		writeError(w, http.StatusConflict, "call already ended")
		return
	}
	cleanTranscript := strings.TrimSpace(transcript)
	if cleanTranscript == "" {
		cleanTranscript = "[Silence / no audio detected]"
	}
	log.Printf("[%s] customer: %q", callID, cleanTranscript)

	// Append customer turn to history.
	sess.History = append(sess.History, ChatMessage{
		Role:    "user",
		Content: cleanTranscript,
	})
	sess.Transcript = append(sess.Transcript, TurnEntry{
		Role: "customer", Text: cleanTranscript, Timestamp: time.Now(),
	})

	// LLM decision.
	decision, err := s.groq.Chat(sess.History)
	if err != nil {
		log.Printf("[%s] chat error: %v", callID, err)
		writeError(w, http.StatusInternalServerError, "LLM error: "+err.Error())
		return
	}
	log.Printf("[%s] bot decision: status=%s disposition=%v reasoning=%q reply=%q",
		callID, decision.Status, decision.Disposition, decision.Reasoning, decision.Reply)

	// TTS — only if there is a reply.
	var audioB64 string
	if decision.Reply != "" {
		audioB64, err = s.groq.SpeakBase64(decision.Reply)
		if err != nil {
			log.Printf("[%s] TTS error: %v", callID, err)
			// Non-fatal.
		}
		sess.History = append(sess.History, ChatMessage{
			Role:    "assistant",
			Content: decision.Reply,
		})
		sess.Transcript = append(sess.Transcript, TurnEntry{
			Role: "bot", Text: decision.Reply, Timestamp: time.Now(),
		})
	}

	// Mark session ended if the LLM decided so.
	if decision.Status == "ended" && decision.Disposition != nil {
		cleanDisp := strings.TrimSpace(*decision.Disposition)
		decision.Disposition = &cleanDisp
		s.closeSession(sess, cleanDisp)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transcript_text":   transcript,
		"reply_text":        decision.Reply,
		"reply_audio_base64": audioB64,
		"status":            decision.Status,
		"disposition":       decision.Disposition,
		"reasoning":         decision.Reasoning,
	})
}

// POST /api/call/hangup — body: { "call_id": "...", "disposition": "NI" }
func (s *Server) handleCallHangup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallID      string `json:"call_id"`
		Disposition string `json:"disposition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}

	s.mu.Lock()
	sess, ok := s.sessions[req.CallID]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "unknown call_id")
		return
	}

	code := req.Disposition
	if code == "" {
		code = "CH" // default manual hangup
	}
	s.closeSession(sess, code)
	log.Printf("[%s] manual hangup — disposition: %s", sess.ID, code)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"call_id":     sess.ID,
		"disposition": code,
		"status":      "ended",
	})
}

// GET /api/call/history
func (s *Server) handleCallHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	type historyItem struct {
		ID          string     `json:"id"`
		PhoneNumber string     `json:"phone_number"`
		StartedAt   time.Time  `json:"started_at"`
		EndedAt     *time.Time `json:"ended_at,omitempty"`
		Status      string     `json:"status"`
		Disposition *string    `json:"disposition,omitempty"`
		Transcript  []TurnEntry `json:"transcript"`
	}

	items := make([]historyItem, 0, len(s.history))
	for _, sess := range s.history {
		items = append(items, historyItem{
			ID:          sess.ID,
			PhoneNumber: sess.PhoneNumber,
			StartedAt:   sess.StartedAt,
			EndedAt:     sess.EndedAt,
			Status:      sess.Status,
			Disposition: sess.Disposition,
			Transcript:  sess.Transcript,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

var fakeAreaCodes = []string{"305", "407", "954", "786", "561", "813", "727"}

func (s *Server) newSession() *CallSession {
	id := fmt.Sprintf("call_%d", time.Now().UnixNano())
	number := fmt.Sprintf("+1 (%s) %03d-%04d",
		fakeAreaCodes[rand.Intn(len(fakeAreaCodes))],
		rand.Intn(900)+100,
		rand.Intn(9000)+1000,
	)
	sess := &CallSession{
		ID:          id,
		PhoneNumber: number,
		StartedAt:   time.Now(),
		Status:      "active",
		History: []ChatMessage{
			{Role: "system", Content: s.sysPrompt},
		},
	}
	s.mu.Lock()
	s.history = append(s.history, sess)
	s.mu.Unlock()
	return sess
}

func (s *Server) closeSession(sess *CallSession, disposition string) {
	now := time.Now()
	sess.Status = "ended"
	sess.Disposition = &disposition
	sess.EndedAt = &now
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// isWhisperHallucination identifies known phantom transcripts generated by Whisper on silence or background noise.
// IMPORTANT: Only filter phrases that Whisper hallucinates on blank/silent audio.
// NEVER filter short valid responses like "yes", "no", "okay", "sure", "hello" — those are real answers!
func isWhisperHallucination(text string) bool {
	t := strings.Trim(strings.ToLower(strings.TrimSpace(text)), ".,!?- \"'()")
	if t == "" {
		return true
	}
	// Only filter known Whisper phantom artifacts on silence/ambient audio
	// These are strings Whisper generates when there is NO real speech
	hallucinations := []string{
		"thank you for watching", "thanks for watching", "please subscribe",
		"like and subscribe", "don't forget to subscribe",
		"subtitles by", "translated by", "captioning provided",
		"amara.org", "community captions",
	}
	for _, h := range hallucinations {
		if t == h || strings.HasPrefix(t, "subtitles by") || strings.HasPrefix(t, "amara.org") {
			return true
		}
	}
	// Filter pure noise filler — ONLY if it's a single isolated token with no real content
	// (e.g. Whisper outputting "Hmm" or "Uh" on 300ms of breathing)
	noiseFiller := []string{"um", "uh", "hmm", "hm", "mm", "mhm"}
	for _, n := range noiseFiller {
		if t == n {
			return true
		}
	}
	return false
}




// isAcousticEcho checks if customer audio is just the laptop mic recording the bot's own speaker output.
func isAcousticEcho(customerText string, botHistory []string) bool {
	cleanCust := strings.Trim(strings.ToLower(strings.TrimSpace(customerText)), ".,!?- \"'")
	if cleanCust == "" {
		return true
	}
	custWords := strings.Fields(cleanCust)
	if len(custWords) == 0 {
		return true
	}

	for _, botText := range botHistory {
		cleanBot := strings.Trim(strings.ToLower(strings.TrimSpace(botText)), ".,!?- \"'")
		if cleanBot == "" {
			continue
		}

		// Exact substring containment
		if strings.Contains(cleanBot, cleanCust) || strings.Contains(cleanCust, cleanBot) {
			return true
		}

		// Word overlap calculation
		botWordMap := make(map[string]bool)
		for _, w := range strings.Fields(cleanBot) {
			wClean := strings.Trim(w, ".,!?- \"'")
			if len(wClean) > 2 {
				botWordMap[wClean] = true
			}
		}

		matched := 0
		for _, cw := range custWords {
			cwClean := strings.Trim(cw, ".,!?- \"'")
			if len(cwClean) > 2 && botWordMap[cwClean] {
				matched++
			}
		}

		// If more than 40% of words overlap with the bot's speech, it's acoustic echo
		if len(custWords) > 1 && float64(matched)/float64(len(custWords)) >= 0.40 {
			return true
		}
		// Single word matching specific keywords from bot pitch
		if len(custWords) == 1 && matched == 1 && (cleanCust == "insurance" || cleanCust == "homeshield" || cleanCust == "homeowner" || cleanCust == "licensed" || cleanCust == "rates") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// .env loader
// ---------------------------------------------------------------------------

// loadDotEnv reads KEY=VALUE pairs from a .env file and sets them as
// environment variables. Silently skips missing file (key may already be set).
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // .env is optional if key is already in the environment
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Don't overwrite existing env vars (shell export takes priority).
		if os.Getenv(key) == "" {
			_ = os.Setenv(key, val)
		}
	}
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	loadDotEnv(".env")

	// Parse multi-key pool or single key
	rawKeys := os.Getenv("GEMINI_API_KEYS")
	var keyList []string
	if rawKeys != "" {
		for _, part := range strings.FieldsFunc(rawKeys, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r'
		}) {
			p := strings.TrimSpace(part)
			if p != "" {
				keyList = append(keyList, p)
			}
		}
	}

	singleKey := os.Getenv("GEMINI_API_KEY")
	if len(keyList) == 0 && singleKey != "" {
		keyList = append(keyList, singleKey)
	}

	voiceEngine := strings.ToLower(os.Getenv("VOICE_ENGINE"))
	if voiceEngine == "" {
		if len(keyList) > 0 {
			voiceEngine = "gemini"
		} else {
			voiceEngine = "groq"
		}
	}

	groq, err := NewGroqClient()
	if err != nil {
		if len(keyList) > 0 {
			log.Printf("Notice: Groq not configured (%v), running with Gemini Live API", err)
		} else {
			log.Printf("Warning: Groq client init: %v. Please configure GEMINI_API_KEYS or GROQ_API_KEY in .env", err)
		}
	}

	srv := NewServer(groq)
	srv.voiceEngine = voiceEngine
	srv.geminiKeyPool = NewKeyPool(keyList)
	if len(keyList) > 0 {
		srv.geminiApiKey = keyList[0]
	}
	srv.geminiVoice = os.Getenv("GEMINI_LIVE_VOICE")
	if srv.geminiVoice == "" {
		srv.geminiVoice = "Aoede"
	}
	srv.geminiModel = os.Getenv("GEMINI_LIVE_MODEL")
	if srv.geminiModel == "" {
		srv.geminiModel = "models/gemini-2.5-flash-native-audio-latest"
	}
	srv.geminiSysPrompt = BuildGeminiLiveSystemPrompt(DefaultCampaignScript, "Sarah")

	addr := ":8080"
	url := fmt.Sprintf("http://localhost%s", addr)
	log.Printf("VICIdial Simulator running — open %s in your browser", url)

	if voiceEngine == "gemini" {
		log.Printf("⚡ Primary Voice Engine: Gemini Live API (Model: %s | Voice: %s)", srv.geminiModel, srv.geminiVoice)
		if srv.geminiKeyPool.Size() == 0 {
			log.Printf("⚠️ GEMINI_API_KEYS is not set in .env! Add GEMINI_API_KEYS=key1,key2... to .env")
		} else {
			log.Printf("✅ Gemini Key Pool active: %d keys loaded (%d RPM / %d Daily Calls at $0.00)",
				srv.geminiKeyPool.Size(), srv.geminiKeyPool.Size()*15, srv.geminiKeyPool.Size()*1500)
		}
	} else {
		log.Printf("Primary Voice Engine: Groq Turn-Based (Chat + Whisper STT + Orpheus TTS)")
	}

	// Automatically open the browser in a tab once the server starts
	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser(url)
	}()

	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
