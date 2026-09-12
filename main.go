package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type Bot struct {
	Name       string `json:"name"`
	LastName   string `json:"last_name"`
	Company    string `json:"company"`
	MaxSeconds int    `json:"max_seconds"`
}

type Session struct {
	ID          string    `json:"id"`
	Bot         Bot       `json:"bot"`
	StartedAt   time.Time `json:"started_at"`
	Turns       []Turn    `json:"turns"`
	Step        string    `json:"step"`
	Age         int       `json:"age,omitempty"`
	BirthYear   int       `json:"birth_year,omitempty"`
	Coverage    string    `json:"coverage,omitempty"`
	OffTopic    int       `json:"off_topic_count,omitempty"`
	Disposition string    `json:"disposition,omitempty"`
	Ended       bool      `json:"ended"`
}

type Turn struct {
	Role        string `json:"role"`
	Text        string `json:"text"`
	AudioFile   string `json:"audio_file,omitempty"`
	Action      string `json:"action,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

type App struct {
	mu              sync.RWMutex
	voiceMu         sync.RWMutex
	sessions        map[string]*Session
	voices          string
	voiceModel      string
	noAnswerSeconds int
}

var defaultBot = Bot{Name: env("BOT_FULL_NAME", "Daniel Brooks"), LastName: env("BOT_LAST_NAME", "Brooks"), Company: env("BOT_COMPANY", "American Resource Center"), MaxSeconds: 90}

// Testing defaults to neutral persona wording. Set AUTOMATED_CALL_DISCLOSURE=1
// for the final compliant product build and regenerate the matching clips.
func automatedDisclosureEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AUTOMATED_CALL_DISCLOSURE")))
	return v == "1" || v == "true" || v == "yes"
}

func assistantIdentity(kind string) string {
	if automatedDisclosureEnabled() {
		switch kind {
		case "identity":
			return "I’m " + defaultBot.Name + ", an automated calling assistant with " + defaultBot.Company + ". I can help with the initial questions, and a licensed specialist can provide the full details."
		case "last_name":
			return "My name is " + defaultBot.Name + ". I’m an automated calling assistant, not a licensed agent. A licensed specialist can identify themselves when I connect you."
		default:
			return "I’m " + defaultBot.Name + ", an automated calling assistant helping with the initial questions. A licensed specialist can provide the full details, and I’ll keep this brief."
		}
	}
	switch kind {
	case "identity":
		return "I’m " + defaultBot.Name + " with " + defaultBot.Company + ". I can help with the initial questions, and a licensed specialist can provide the full details."
	case "last_name":
		return "My name is " + defaultBot.Name + ". I can help with the initial questions, and a licensed specialist can identify themselves when I connect you."
	default:
		return "I’m " + defaultBot.Name + " with " + defaultBot.Company + ". I handle the initial questions, and a licensed specialist can provide the full details."
	}
}

func (a *App) reapSessions() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-24 * time.Hour)
		a.mu.Lock()
		for id, session := range a.sessions {
			if session.StartedAt.Before(cutoff) {
				delete(a.sessions, id)
			}
		}
		a.mu.Unlock()
	}
}

func main() {
	app := &App{
		sessions:        map[string]*Session{},
		voices:          env("VOICES_DIR", "voices"),
		voiceModel:      safeVoiceFolder(env("VOICE_MODEL", "aura-2-orpheus-en")),
		noAnswerSeconds: envInt("NO_ANSWER_SECONDS", 8, 3, 60),
	}
	go app.reapSessions()
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.page)
	mux.HandleFunc("/static/", app.static)
	mux.HandleFunc("/voices/", app.voice)
	mux.HandleFunc("/api/health", app.health)
	mux.HandleFunc("/api/stats", app.stats)
	mux.HandleFunc("/api/config", app.config)
	mux.HandleFunc("/api/voices", app.voicesList)
	mux.HandleFunc("/api/config/voice", app.setVoice)
	mux.HandleFunc("/api/calls/start", app.start)
	mux.HandleFunc("/api/calls/turn", app.turn)
	mux.HandleFunc("/api/calls/no-answer", app.noAnswer)
	mux.HandleFunc("/api/calls/hangup", app.hangup)
	mux.HandleFunc("/api/vicidial/disposition", app.vicidialDisposition)
	mux.HandleFunc("/api/vicidial/transfer", app.vicidialTransfer)
	mux.HandleFunc("/api/vicidial/amd", app.vicidialAMD)

	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("VICIdial Bot listening on %s", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           logging(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, "web/index.html")
}

func (a *App) static(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.StripPrefix("/static/", http.FileServer(http.Dir("web/static"))).ServeHTTP(w, r)
}

func (a *App) voice(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/voices/")
	parts := strings.Split(name, "/")
	if name == "" || len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/\\`) {
			http.NotFound(w, r)
			return
		}
	}
	path := filepath.Join(append([]string{a.voices}, parts...)...)
	root, rootErr := filepath.Abs(a.voices)
	target, targetErr := filepath.Abs(path)
	if rootErr != nil || targetErr != nil || !strings.HasPrefix(target, root+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"ok": true, "service": "vicidial-bot", "time": time.Now().UTC()})
}

var traffic struct {
	sync.Mutex
	bytes uint64
	at    time.Time
}

func (a *App) stats(w http.ResponseWriter, r *http.Request) {
	cpu, ram := processUsage()
	traffic.Lock()
	now := time.Now()
	if traffic.at.IsZero() {
		traffic.at = now
	}
	seconds := now.Sub(traffic.at).Seconds()
	kbps := float64(0)
	if seconds > 0 {
		kbps = float64(traffic.bytes) / 1024 / seconds
	}
	traffic.bytes = 0
	traffic.at = now
	traffic.Unlock()
	a.mu.RLock()
	active := 0
	for _, session := range a.sessions {
		if !session.Ended {
			active++
		}
	}
	a.mu.RUnlock()
	jsonOut(w, map[string]any{"cpu_percent": cpu, "ram_mb": ram, "network_kbps": kbps, "active_calls": active, "goroutines": runtime.NumGoroutine()})
}

func processUsage() (float64, float64) {
	output, err := exec.Command("ps", "-o", "%cpu=", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err == nil {
		fields := strings.Fields(string(output))
		if len(fields) >= 2 {
			cpu, cpuErr := strconv.ParseFloat(fields[0], 64)
			rss, rssErr := strconv.ParseFloat(fields[1], 64)
			if cpuErr == nil && rssErr == nil {
				return cpu, rss / 1024
			}
		}
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return 0, float64(memory.Sys) / (1024 * 1024)
}

func (a *App) config(w http.ResponseWriter, r *http.Request) {
	jsonOut(w, map[string]any{"bot": defaultBot, "engine": "deterministic-script", "stt": "local-vosk-streaming", "tts": "pre-recorded-voice", "voice_model": a.selectedVoice(), "interruption": "local-rms-vad", "max_seconds": defaultBot.MaxSeconds, "no_answer_timeout_seconds": a.noAnswerSeconds})
}

type voiceOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Clips int    `json:"clips"`
}

func (a *App) availableVoices() []voiceOption {
	entries, err := os.ReadDir(a.voices)
	if err != nil {
		return nil
	}
	voices := make([]voiceOption, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, "aura-2-") || !strings.HasSuffix(name, "-en") {
			continue
		}
		manifestPath := filepath.Join(a.voices, name, "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest struct {
			Files []string `json:"files"`
		}
		if json.Unmarshal(data, &manifest) != nil || len(manifest.Files) == 0 {
			continue
		}
		complete := true
		for _, file := range manifest.Files {
			if file == "" || filepath.Base(file) != file {
				complete = false
				break
			}
			if info, statErr := os.Stat(filepath.Join(a.voices, name, file)); statErr != nil || !info.Mode().IsRegular() {
				complete = false
				break
			}
		}
		if complete {
			voices = append(voices, voiceOption{ID: name, Label: voiceLabel(name), Clips: len(manifest.Files)})
		}
	}
	sort.Slice(voices, func(i, j int) bool { return voices[i].ID < voices[j].ID })
	return voices
}

func voiceLabel(model string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(model, "aura-2-"), "-en")
	words := strings.Split(name, "-")
	for i, word := range words {
		if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ") + " — Aura 2 English"
}

func (a *App) selectedVoice() string {
	a.voiceMu.RLock()
	defer a.voiceMu.RUnlock()
	return a.voiceModel
}

func (a *App) voicesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	jsonOut(w, map[string]any{"selected": a.selectedVoice(), "voices": a.availableVoices()})
}

func (a *App) setVoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		VoiceModel string `json:"voice_model"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.VoiceModel) == "" {
		http.Error(w, "voice_model required", http.StatusBadRequest)
		return
	}
	model := safeVoiceFolder(req.VoiceModel)
	valid := false
	for _, voice := range a.availableVoices() {
		if voice.ID == model {
			valid = true
			break
		}
	}
	if !valid {
		http.Error(w, "voice model is not available", http.StatusBadRequest)
		return
	}
	a.voiceMu.Lock()
	a.voiceModel = model
	a.voiceMu.Unlock()
	jsonOut(w, map[string]any{"ok": true, "voice_model": model})
}

func safeVoiceFolder(model string) string {
	clean := filepath.Base(filepath.Clean(model))
	if clean != "aura-2-orpheus-en" || strings.ContainsAny(clean, `/\\`) {
		return "aura-2-orpheus-en"
	}
	return clean
}

func (a *App) withAudio(result classification) classification {
	if len(result.AudioFiles) > 0 {
		files := make([]string, 0, len(result.AudioFiles))
		for _, file := range result.AudioFiles {
			if file == "" || strings.Contains(file, "/") {
				continue
			}
			voicePath := filepath.Join(a.voices, a.selectedVoice(), file)
			if _, err := os.Stat(voicePath); err == nil {
				files = append(files, a.selectedVoice()+"/"+file)
			}
		}
		result.AudioFiles = files
		return result
	}
	if result.Audio == "" || strings.Contains(result.Audio, "/") {
		return result
	}
	model := a.selectedVoice()
	voicePath := filepath.Join(a.voices, model, result.Audio)
	if _, err := os.Stat(voicePath); err == nil {
		result.Audio = model + "/" + result.Audio
	} else if result.Audio == "no_answer.wav" {
		// No-answer can be returned even when older voice batches predate this
		// clip. Do not play an unrelated sentence in its place.
		result.Audio = ""
	}
	return result
}

func (a *App) start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	id := newID()
	s := &Session{ID: id, Bot: defaultBot, StartedAt: time.Now().UTC(), Step: "age"}
	a.mu.Lock()
	a.sessions[id] = s
	a.mu.Unlock()
	respond(w, map[string]any{"session_id": id, "bot": defaultBot, "reply": a.withAudio(reply("opening", "")), "state": "listening", "no_answer_timeout_seconds": a.noAnswerSeconds})
}

type turnRequest struct {
	SessionID string `json:"session_id"`
	Text      string `json:"text"`
}

func (a *App) turn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var req turnRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.SessionID) == "" {
		http.Error(w, "session_id and text required", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len([]rune(text)) > 1000 {
		http.Error(w, "text must be between 1 and 1000 characters", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	if s == nil {
		a.mu.Unlock()
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if s.Ended {
		a.mu.Unlock()
		http.Error(w, "session already ended", http.StatusConflict)
		return
	}
	result, nextStep := classify(s, text)
	if time.Since(s.StartedAt) >= time.Duration(s.Bot.MaxSeconds)*time.Second {
		result = classification{Text: "Thank you. We have reached the call time limit. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "CALL_LIMIT"}
		nextStep = "done"
	}
	result = a.withAudio(result)
	if nextStep != "" {
		s.Step = nextStep
	}
	s.Turns = append(s.Turns, Turn{Role: "customer", Text: text})
	botTurn := Turn{Role: "bot", Text: result.Text, AudioFile: result.Audio, Action: result.Action, Disposition: result.Disposition}
	s.Turns = append(s.Turns, botTurn)
	if result.Disposition != "" {
		s.Disposition, s.Ended = result.Disposition, true
	}
	snapshot := *s
	snapshot.Turns = append([]Turn(nil), s.Turns...)
	a.mu.Unlock()
	respond(w, map[string]any{"session_id": snapshot.ID, "reply": result, "session": snapshot})
}

func (a *App) noAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.SessionID) == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	if s == nil {
		a.mu.Unlock()
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	if s.Ended {
		snapshot := *s
		snapshot.Turns = append([]Turn(nil), s.Turns...)
		a.mu.Unlock()
		respond(w, map[string]any{"session_id": snapshot.ID, "reply": classification{Action: "DISPOSE", Disposition: snapshot.Disposition}, "session": snapshot})
		return
	}
	result := a.withAudio(classification{
		Text:        "I’m sorry, I’m not hearing a response. We’ll mark this as no answer. Goodbye.",
		Audio:       "no_answer.wav",
		Action:      "DISPOSE",
		Disposition: "NO_ANSWER",
	})
	s.Step = "done"
	s.Disposition = result.Disposition
	s.Ended = true
	s.Turns = append(s.Turns, Turn{Role: "bot", Text: result.Text, AudioFile: result.Audio, Action: result.Action, Disposition: result.Disposition})
	snapshot := *s
	snapshot.Turns = append([]Turn(nil), s.Turns...)
	a.mu.Unlock()
	respond(w, map[string]any{"session_id": snapshot.ID, "reply": result, "session": snapshot})
}

func (a *App) hangup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID   string `json:"session_id"`
		Disposition string `json:"disposition"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.sessions[req.SessionID]
	if s != nil {
		s.Ended = true
		if req.Disposition != "" {
			s.Disposition = req.Disposition
		} else {
			s.Disposition = "CxHANG"
		}
	}
	a.mu.Unlock()
	respond(w, map[string]any{"ok": s != nil, "disposition": func() string {
		if s == nil {
			return ""
		}
		return s.Disposition
	}()})
}

func (a *App) vicidialDisposition(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// This endpoint is intentionally provider-neutral. An AGI/AMI adapter can
	// call it after each session and map the returned code to the customer's VICIdial status.
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		Note      string `json:"note"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Code == "" {
		http.Error(w, "session_id and code required", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	if s := a.sessions[req.SessionID]; s != nil {
		s.Disposition = req.Code
		s.Ended = true
	}
	a.mu.Unlock()
	respond(w, map[string]any{"ok": true, "vicidial_code": req.Code, "note": req.Note})
}

func (a *App) vicidialTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		SessionID string `json:"session_id"`
		Queue     string `json:"queue"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	respond(w, map[string]any{"ok": true, "action": "TRANSFER", "session_id": req.SessionID, "queue": req.Queue})
}

// vicidialAMD gives an AMI/ARI bridge a provider-neutral way to report the
// result of Asterisk AMD. The dialplan can also dispose MACHINE directly.
func (a *App) vicidialAMD(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		CallID string `json:"call_id"`
		Status string `json:"status"`
		Cause  string `json:"cause"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.Status) == "" {
		http.Error(w, "status required", http.StatusBadRequest)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(req.Status))
	disposition := ""
	if status == "MACHINE" {
		disposition = "AMD"
	} else if status == "HANGUP" {
		disposition = "NO_ANSWER"
	}
	respond(w, map[string]any{"ok": true, "call_id": req.CallID, "amd_status": status, "amd_cause": req.Cause, "disposition": disposition, "dispose": disposition != ""})
}

type classification struct {
	Text        string   `json:"text"`
	Audio       string   `json:"audio_file"`
	AudioFiles  []string `json:"audio_files,omitempty"`
	Action      string   `json:"action,omitempty"`
	Disposition string   `json:"disposition,omitempty"`
}

func classify(s *Session, input string) (classification, string) {
	l := strings.ToLower(input)
	switch {
	case hasAbusiveLanguage(l):
		return classification{Text: "Understood. We will not contact you again. Goodbye.", Audio: "dnc.wav", Action: "DNC", Disposition: "DNC"}, "done"
	case has(l, "not interested") && has(l, "do not call", "don't call", "stop calling", "call again"):
		return classification{Text: "No problem. Thank you for your time. We will mark this as not interested. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
	case has(l,
		"business number", "business line", "company number", "office number", "work number", "work phone",
		"this is a business", "this is the office", "this is a workplace", "this is a work phone",
		"this is a school", "this is a salon", "this is a clinic", "this is a store", "this is a shop",
		"this is a restaurant", "this is a hotel", "workplace", "school", "salon", "clinic", "store", "shop",
		"not home", "not a home", "not my home", "not at home", "not residential"):
		return classification{Text: "Thanks for letting me know. We will mark this as a business or non-residential number. Goodbye.", Audio: "dnc.wav", Action: "DISPOSE", Disposition: "BUSINESS_NUMBER"}, "done"
	case has(l, "do not call", "don't call", "remove my number", "stop calling", "unsubscribe"):
		return classification{Text: "Understood. We will not contact you again. Goodbye.", Audio: "dnc.wav", Action: "DNC", Disposition: "DNC"}, "done"
	case has(l, "wrong number", "not this person"):
		return classification{Text: "I am sorry about that. We will update our records. Goodbye.", Audio: "wrong_number.wav", Action: "DISPOSE", Disposition: "WRONG_NUMBER"}, "done"
	case has(l, "busy", "can't talk", "cannot talk"):
		return classification{Text: "No problem. I understand you are busy. Goodbye.", Audio: "busy_ack.wav", Action: "DISPOSE", Disposition: "BUSY"}, "done"
	case has(l, "call me later", "call back", "callback", "another time"):
		return classification{Text: "Of course. We will arrange a callback. Thank you.", Audio: "callback.wav", Action: "CALLBACK", Disposition: "CALLBK"}, "done"
	case has(l, "not interested", "not for me", "leave me alone"):
		return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
	}
	// An explicit request to be connected takes priority over an identity
	// question such as "are you a real person?".
	if has(l, "connect me", "put me through", "transfer me", "let me talk", "speak to") && has(l, "human", "real person", "agent", "representative", "specialist") {
		return classification{Text: "Certainly. Please hold while I connect you.", Audio: "transfer.wav", Action: "TRANSFER", Disposition: "RXFER"}, "done"
	}
	// Answer identity questions before the generic transfer keywords. For
	// example, "Are you a real person?" is a question about the bot, not a
	// request to transfer to a human agent.
	if question, ok := answerQuestion(s, l); ok {
		s.OffTopic++
		if s.OffTopic >= 4 {
			return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
		}
		return question, s.Step
	}
	switch {
	case has(l, "human", "person", "agent", "representative", "specialist"):
		return classification{Text: "Certainly. Please hold while I connect you.", Audio: "transfer.wav", Action: "TRANSFER", Disposition: "RXFER"}, "done"
	}

	switch s.Step {
	case "age":
		age, birthYear, ok := parseAge(l)
		if !ok {
			s.OffTopic++
			if s.OffTopic >= 3 {
				return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
			}
			return classification{Text: "I just need your age to check the available options. How old are you?", Audio: "age_reask.wav", Action: "REASK"}, "age"
		}
		s.OffTopic = 0
		s.Age = age
		s.BirthYear = birthYear
		if age < 50 || age > 80 {
			return classification{Text: "Thank you for letting me know. These options are currently for people between 50 and 80. I appreciate your time. Goodbye.", Audio: "not_eligible.wav", Action: "DISPOSE", Disposition: "NOT_ELIGIBLE"}, "done"
		}
		return classification{Text: "Perfect, thank you. That’s exactly what I needed to confirm. There may be some options available for you based on your age, and the person who can actually go over those details with you is one of our licensed specialists. I’ll have them take it from here so you can get the information directly from someone who can answer your questions. Just stay with me for a moment while I get you over to the specialist, and they’ll pick it up from here.", AudioFiles: []string{"acknowledge_natural.wav", "age_confirm.wav", "age_handoff.wav"}, Action: "TRANSFER", Disposition: "RXFER"}, "done"
	case "coverage":
		if has(l, "yes", "i do", "covered", "have coverage", "receiving", "benefit", "insurance", "medicare", "medicaid", "policy", "insured", "plan") {
			s.Coverage = "yes"
		}
		if has(l, "no", "not", "none", "don't", "dont", "nothing") {
			s.Coverage = "no"
		}
		if s.Coverage == "" {
			s.OffTopic++
			if s.OffTopic >= 3 {
				return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
			}
			return classification{Text: "No problem. Just to make sure I have it right, are you currently receiving any coverage or benefits that would help your family with expenses at the time of death?", Audio: "coverage_question.wav", Action: "REASK"}, "coverage"
		}
		s.OffTopic = 0
		return classification{Text: "The reason I’m asking is that there are coverage options designed to help families with those costs, and a licensed specialist can check what you may be eligible for. I can transfer you now so they can go over the details with you. Is that okay?", Audio: "transfer_offer.wav", Action: "CONTINUE"}, "transfer_confirm"
	case "transfer_confirm":
		if has(l, "yes", "okay", "ok", "sure", "go ahead", "that is okay", "please") {
			return classification{Text: "Great, thank you. Please hold while I connect you with a licensed specialist.", Audio: "transfer_confirm.wav", Action: "TRANSFER", Disposition: "RXFER"}, "done"
		}
		if has(l, "no", "not interested", "no thanks", "decline") {
			s.OffTopic = 0
			return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
		}
		s.OffTopic++
		if s.OffTopic >= 3 {
			return classification{Text: "No problem. Thank you for your time. Goodbye.", Audio: "not_interested.wav", Action: "DISPOSE", Disposition: "NI"}, "done"
		}
		return classification{Text: "Perfect, thank you. That’s exactly what I needed to confirm. There may be some options available for you based on your age, and the person who can actually go over those details with you is one of our licensed specialists. I’ll have them take it from here so you can get the information directly from someone who can answer your questions. Just stay with me for a moment while I get you over to the specialist, and they’ll pick it up from here.", AudioFiles: []string{"acknowledge_natural.wav", "age_confirm.wav", "age_handoff.wav"}, Action: "TRANSFER", Disposition: "RXFER"}, "done"
	default:
		return classification{Text: "I’m sorry, could you repeat that?", Audio: "unclear.wav", Action: "REASK"}, s.Step
	}
}

func reply(kind, _ string) classification {
	if kind == "opening" {
		return classification{Text: "Hi, this is " + defaultBot.Name + " with " + defaultBot.Company + ". I’m reaching out because there may be a benefit available for people in your age group, and I just need to confirm one quick thing before I explain why I’m calling. Are you between 50 and 80?", Audio: "opening_benefits.wav", Action: "CONTINUE"}
	}
	return classification{}
}

func parseAge(input string) (int, int, bool) {
	currentYear := time.Now().Year()
	numeric := strings.FieldsFunc(input, func(r rune) bool { return !unicode.IsDigit(r) })
	for _, token := range numeric {
		if n, err := strconv.Atoi(token); err == nil {
			if n >= 1900 && n <= currentYear && has(input, "born", "birth", "year") {
				return currentYear - n, n, true
			}
			if n >= 18 && n <= 120 {
				return n, 0, true
			}
		}
	}
	tens := map[string]int{
		"forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
		"forties": 40, "fifties": 50, "sixties": 60, "seventies": 70, "eighties": 80, "nineties": 90,
	}
	ones := map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9}
	words := strings.FieldsFunc(input, func(r rune) bool { return !unicode.IsLetter(r) })
	for _, word := range words {
		if n, ok := tens[word]; ok {
			for _, one := range words {
				if n2, ok := ones[one]; ok {
					return n + n2, 0, true
				}
			}
			return n, 0, true
		}
	}
	return 0, 0, false
}

func answerQuestion(s *Session, input string) (classification, bool) {
	switch {
	case has(input, "where are you from", "where are you calling from", "which company", "what company", "who do you work for", "who is this", "who are you", "what is your name", "what's your name", "what is your first name"):
		return classification{Text: assistantIdentity("identity"), Audio: "identity.wav", Action: "CONTINUE"}, true
	case has(input, "your last name", "your surname", "family name", "what is your last name", "what's your last name"):
		return classification{Text: assistantIdentity("last_name"), Audio: "last_name.wav", Action: "CONTINUE"}, true
	case has(input, "are you a bot", "are you human", "real person", "robot", "artificial intelligence", "are you automated", "what is your role") || hasWord(input, "ai"):
		return classification{Text: assistantIdentity("bot_identity"), Audio: "bot_identity.wav", Action: "CONTINUE"}, true
	case hasAll(input, "what", "details") || hasAll(input, "what", "information") || hasAll(input, "what", "exactly") || (has(input, "what") && has(input, "want", "need") && has(input, "do", "looking", "asking")):
		return classification{Text: "I only need a couple of basic answers to see whether a licensed specialist should review the available options with you.", AudioFiles: []string{naturalFiller(), "information_needed.wav"}, Action: "CONTINUE"}, true
	case has(input, "what is this about", "what are you calling about", "what do you want", "why are you calling"):
		return classification{Text: "It’s about benefits and coverage options that may be available for people in your age group. I only need to verify a couple of details.", Audio: "what_this_is.wav", Action: "CONTINUE"}, true
	case has(input, "where is your office", "what city", "what state", "are you in the us", "are you in usa", "are you in america", "where is your contact center"):
		return classification{Text: "I’m calling on behalf of Your Company from its contact center. A licensed specialist can provide the official company details.", Audio: "location.wav", Action: "CONTINUE"}, true
	case has(input, "how long", "how much time", "is this quick", "are you busy"):
		return classification{Text: "It should only take a moment. I’ll ask a couple of questions, and you can stop at any time.", Audio: "time_required.wav", Action: "CONTINUE"}, true
	case has(input, "why do you need", "why ask", "what do you need from me", "what information"):
		return classification{Text: "I only need a couple of basic answers to see whether a licensed specialist should review the available options with you.", AudioFiles: []string{naturalFiller(), "information_needed.wav"}, Action: "CONTINUE"}, true
	case has(input, "repeat", "say that again", "did not hear", "didn't hear", "speak slower"):
		return classification{Text: "Of course. I’ll say it again slowly. I’m checking whether you may qualify for benefits and coverage options, and a licensed specialist can explain the details.", Audio: "repeat_explain.wav", Action: "CONTINUE"}, true
	case has(input, "social security", "ssn", "bank account", "bank details", "credit card", "payment information"):
		return classification{Text: "I do not need your Social Security number, bank details, or payment information. A licensed specialist can explain the official details.", Audio: "privacy.wav", Action: "CONTINUE"}, true
	case has(input, "licensed", "license", "qualified specialist", "who will i speak to"):
		return classification{Text: "A licensed specialist can answer detailed questions and explain any available options.", Audio: "licensed_specialist.wav", Action: "CONTINUE"}, true
	case has(input, "is this required", "do i have to", "no obligation", "obligation"):
		return classification{Text: "There is no obligation to continue. You can stop the call or request a callback at any time.", Audio: "no_pressure.wav", Action: "CONTINUE"}, true
	case has(input, "thank you", "thanks"):
		return classification{Text: "You’re welcome.", Audio: "youre_welcome.wav", Action: "CONTINUE"}, true
	}
	return classification{}, false
}

// naturalFiller chooses a short, pre-generated pause/acknowledgment for
// clarifying questions. It is intentionally limited to safe conversational
// fillers; coughs or laughs are not inserted randomly into a benefits call.
func naturalFiller() string {
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "thinking.wav"
	}
	if b[0]%2 == 0 {
		return "thinking.wav"
	}
	return "warm_ack.wav"
}

func has(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func hasWord(input, wanted string) bool {
	for _, word := range strings.FieldsFunc(strings.ToLower(input), func(r rune) bool { return !unicode.IsLetter(r) }) {
		if word == strings.ToLower(wanted) {
			return true
		}
	}
	return false
}

func hasAll(s string, words ...string) bool {
	for _, word := range words {
		if !strings.Contains(s, word) {
			return false
		}
	}
	return true
}

func hasAbusiveLanguage(input string) bool {
	// Match common profanity as whole words and a few common obfuscated forms.
	// This is deliberately separate from ordinary disagreement or frustration:
	// only clearly abusive/profane language triggers the DNC disposition.
	words := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	bad := map[string]struct{}{
		"fuck": {}, "fucking": {}, "fucker": {}, "motherfucker": {}, "fck": {}, "fuk": {},
		"shit": {}, "shitty": {}, "bullshit": {}, "bitch": {}, "btch": {},
		"ass": {}, "asshole": {}, "assholes": {}, "dumbass": {}, "jackass": {}, "bastard": {}, "dick": {}, "dicks": {},
		"pussy": {}, "cunt": {}, "cunts": {}, "whore": {}, "whores": {}, "slut": {},
		"damn": {}, "dammit": {}, "crap": {}, "piss": {}, "pissed": {}, "stfu": {},
		"idiot": {}, "idiots": {}, "moron": {}, "morons": {}, "stupid": {},
	}
	for _, word := range words {
		if _, ok := bad[word]; ok {
			return true
		}
	}
	// Handle light punctuation/leet obfuscation such as f*** or sh1t without
	// treating normal words containing "ass" as abusive.
	compact := strings.NewReplacer("@", "a", "$", "s", "!", "i", "1", "i", "0", "o", "3", "e").Replace(strings.ToLower(input))
	compact = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, compact)
	for _, root := range []string{"fuck", "fck", "shit", "bitch", "asshole", "bastard", "dick", "pussy", "cunt", "whore", "slut", "bullshit", "stfu"} {
		if strings.Contains(compact, root) {
			return true
		}
	}
	return false
}

func newID() string { b := make([]byte, 8); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func envInt(k string, fallback, min, max int) int {
	v, err := strconv.Atoi(env(k, ""))
	if err != nil {
		return fallback
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func jsonOut(w http.ResponseWriter, v any) { respond(w, v) }
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		counter := &countingWriter{ResponseWriter: w}
		next.ServeHTTP(counter, r)
		traffic.Lock()
		traffic.bytes += uint64(counter.bytes)
		traffic.Unlock()
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

type countingWriter struct {
	http.ResponseWriter
	bytes int
}

func (w *countingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}
