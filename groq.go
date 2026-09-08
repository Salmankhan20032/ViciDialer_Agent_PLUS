package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const groqBaseURL       = "https://api.groq.com/openai/v1"
const openRouterBaseURL = "https://openrouter.ai/api/v1"

// GroqClient holds the Groq API key for STT/TTS and optionally an OpenRouter
// key for chat (giving access to Claude, GPT-4o, Gemini, Mistral, etc.).
type GroqClient struct {
	apiKey          string // Groq key (STT + TTS)
	openRouterKey   string // OpenRouter key (chat) — optional
	httpClient      *http.Client
}

// NewGroqClient creates a GroqClient, reading keys from the environment.
// GROQ_API_KEY is required (STT + TTS). OPENROUTER_API_KEY is optional (chat).
func NewGroqClient() (*GroqClient, error) {
	key := os.Getenv("GROQ_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("GROQ_API_KEY is not set — add it to your .env file and restart")
	}
	orKey := os.Getenv("OPENROUTER_API_KEY")
	if orKey != "" {
		log.Printf("OpenRouter configured — chat will use OpenRouter models")
	} else {
		log.Printf("OpenRouter not configured — chat will use Groq models")
	}
	return &GroqClient{
		apiKey:        key,
		openRouterKey: orKey,
		httpClient:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// ---------------------------------------------------------------------------
// 5.1 Speech-to-Text (Whisper)
// ---------------------------------------------------------------------------

// Transcribe sends audio bytes to Groq Whisper and returns the transcript text.
func (c *GroqClient) Transcribe(audioData []byte, filename string) (string, error) {
	return c.TranscribeWithContext(context.Background(), audioData, filename)
}

// TranscribeWithContext sends audio to Whisper respecting context cancellation.
// Tries the primary STT model, then falls back to alternative models on error.
func (c *GroqClient) TranscribeWithContext(ctx context.Context, audioData []byte, filename string) (string, error) {
	sttModel := os.Getenv("GROQ_STT_MODEL")
	if sttModel == "" {
		sttModel = "whisper-large-v3-turbo"
	}
	// Fallback models if primary is rate-limited or unavailable
	modelsToTry := []string{
		sttModel,
		"distil-whisper-large-v3-en",
		"whisper-large-v3",
	}
	// Deduplicate: if env already set one of the fallbacks as primary, skip it
	seen := map[string]bool{sttModel: true}
	uniqueModels := []string{sttModel}
	for _, m := range modelsToTry[1:] {
		if !seen[m] {
			seen[m] = true
			uniqueModels = append(uniqueModels, m)
		}
	}

	var lastErr error
	for i, model := range uniqueModels {
		result, err := c.transcribeWithModel(ctx, audioData, filename, model)
		if err == nil {
			if i > 0 {
				log.Printf("STT fallback succeeded with model: %s", model)
			}
			return result, nil
		}
		lastErr = err
		// Only retry on rate-limit (429) or server error (5xx), not bad-request
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "503") {
			log.Printf("STT model %s failed (%v), trying fallback...", model, err)
			continue
		}
		return "", err // non-retriable error
	}
	return "", lastErr
}

// transcribeWithModel does the actual Whisper API call for a specific model.
func (c *GroqClient) transcribeWithModel(ctx context.Context, audioData []byte, filename string, sttModel string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	if err := mw.WriteField("model", sttModel); err != nil {
		return "", fmt.Errorf("whisper multipart model field: %w", err)
	}

	// Enforce English to reduce hallucinations and cut latency
	if err := mw.WriteField("language", "en"); err != nil {
		return "", fmt.Errorf("whisper multipart language field: %w", err)
	}
	// Temperature 0.0: deterministic, suppresses hallucinations
	if err := mw.WriteField("temperature", "0.0"); err != nil {
		return "", fmt.Errorf("whisper multipart temperature field: %w", err)
	}
	// Conversational telephony hint
	if err := mw.WriteField("prompt", "Outbound sales phone call, English only."); err != nil {
		return "", fmt.Errorf("whisper multipart prompt field: %w", err)
	}

	ext := filepath.Ext(filename)
	if ext == "" {
		ext = ".webm"
	}
	part, err := mw.CreateFormFile("file", "audio"+ext)
	if err != nil {
		return "", fmt.Errorf("whisper multipart file field: %w", err)
	}
	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("whisper multipart write audio: %w", err)
	}
	mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqBaseURL+"/audio/transcriptions", &buf)
	if err != nil {
		return "", fmt.Errorf("whisper build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper HTTP: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("whisper parse response: %w", err)
	}
	return result.Text, nil
}

// ---------------------------------------------------------------------------
// 5.2 Chat / Decision Engine (LLM)
// ---------------------------------------------------------------------------

// ChatMessage represents a single message in the conversation history.
type ChatMessage struct {
	Role    string `json:"role"`    // "system", "user", "assistant"
	Content string `json:"content"`
}

// BotDecision is the structured JSON the LLM returns on every turn.
type BotDecision struct {
	Reply       string  `json:"reply"`
	Status      string  `json:"status"`      // "ongoing" | "ended"
	Disposition *string `json:"disposition"` // null or disposition code
	Reasoning   string  `json:"reasoning"`
}

// Chat sends the full conversation history to the LLM and returns the bot's decision.
func (c *GroqClient) Chat(messages []ChatMessage) (*BotDecision, error) {
	return c.ChatWithContext(context.Background(), messages)
}

// ChatWithContext routes to OpenRouter (if configured) or Groq with fallbacks.
// OpenRouter gives access to Claude, GPT-4o, Gemini, Mistral, etc.
// Groq is used as fallback when OpenRouter is not set or rate-limited.
func (c *GroqClient) ChatWithContext(ctx context.Context, messages []ChatMessage) (*BotDecision, error) {
	// ── OpenRouter path ──────────────────────────────────────────────────────
	if c.openRouterKey != "" {
		orModel := os.Getenv("OPENROUTER_MODEL")
		if orModel == "" {
			orModel = "meta-llama/llama-3.1-8b-instruct:free" // free tier default
		}
		// OpenRouter model fallback chain
		orModels := []string{
			orModel,
			"meta-llama/llama-3.1-8b-instruct:free",
			"google/gemma-2-9b-it:free",
			"mistralai/mistral-7b-instruct:free",
		}
		seen := map[string]bool{orModel: true}
		uniqueOR := []string{orModel}
		for _, m := range orModels[1:] {
			if !seen[m] {
				seen[m] = true
				uniqueOR = append(uniqueOR, m)
			}
		}
		for i, model := range uniqueOR {
			decision, err := c.chatViaOpenRouter(ctx, messages, model)
			if err == nil {
				if i > 0 {
					log.Printf("OpenRouter fallback succeeded with: %s", model)
				}
				return decision, nil
			}
			if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "503") {
				log.Printf("OpenRouter model %s failed (%v), trying next...", model, err)
				continue
			}
			// Non-retriable: fall through to Groq
			log.Printf("OpenRouter error (non-retriable): %v — falling back to Groq", err)
			break
		}
		log.Printf("All OpenRouter models failed — falling back to Groq")
	}

	// ── Groq path (primary if no OpenRouter, fallback otherwise) ─────────────
	chatModel := os.Getenv("GROQ_CHAT_MODEL")
	if chatModel == "" {
		chatModel = "llama-3.1-8b-instant"
	}
	groqModels := []string{
		chatModel,
		"llama-3.1-8b-instant",
		"gemma2-9b-it",
		"llama3-8b-8192",
	}
	seen := map[string]bool{chatModel: true}
	uniqueGroq := []string{chatModel}
	for _, m := range groqModels[1:] {
		if !seen[m] {
			seen[m] = true
			uniqueGroq = append(uniqueGroq, m)
		}
	}

	var lastErr error
	for i, model := range uniqueGroq {
		decision, err := c.chatWithModel(ctx, messages, model)
		if err == nil {
			if i > 0 {
				log.Printf("Groq fallback succeeded with: %s", model)
			}
			return decision, nil
		}
		lastErr = err
		if strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "503") {
			log.Printf("Groq model %s failed (%v), trying next...", model, err)
			continue
		}
		return nil, err
	}
	return nil, lastErr
}

// chatViaOpenRouter sends chat to OpenRouter's OpenAI-compatible API.
func (c *GroqClient) chatViaOpenRouter(ctx context.Context, messages []ChatMessage, model string) (*BotDecision, error) {
	payload := map[string]interface{}{
		"model":           model,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        messages,
		"temperature":     0.5,
		"max_tokens":      200,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("openrouter marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openrouter build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.openRouterKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HTTP-Referer", "https://vicidialer-sim.local")
	req.Header.Set("X-Title", "VICIdial AI Agent")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter HTTP: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter API error %d: %s", resp.StatusCode, string(respBody))
	}

	var envelope struct {
		Choices []struct {
			Message struct{ Content string `json:"content"` } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("openrouter parse: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("openrouter: no choices")
	}

	var decision BotDecision
	content := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(content), &decision); err != nil {
		// Try to extract JSON from within
		start := strings.Index(content, "{")
		end := strings.LastIndex(content, "}")
		if start != -1 && end > start {
			_ = json.Unmarshal([]byte(content[start:end+1]), &decision)
		}
		if decision.Reply == "" {
			decision.Reply = content
			decision.Status = "ongoing"
		}
	}
	if decision.Status != "ongoing" && decision.Status != "ended" {
		decision.Status = "ongoing"
	}
	return &decision, nil
}


// chatWithModel does the actual LLM API call for a specific model.
func (c *GroqClient) chatWithModel(ctx context.Context, messages []ChatMessage, chatModel string) (*BotDecision, error) {
	payload := map[string]interface{}{
		"model":           chatModel,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        messages,
		"temperature":     0.5,
		"max_tokens":      180,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("chat marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("chat build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chat HTTP: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Resilient recovery: extract text from failed_generation if JSON mode failed
		var errResp struct {
			Error struct {
				Code             string `json:"code"`
				FailedGeneration string `json:"failed_generation"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error.FailedGeneration != "" {
			cleanText := strings.TrimSpace(errResp.Error.FailedGeneration)
			cleanText = strings.Trim(cleanText, "\"")
			return &BotDecision{
				Reply:     cleanText,
				Status:    "ongoing",
				Reasoning: "Recovered from model generation",
			}, nil
		}
		return nil, fmt.Errorf("chat API error %d: %s", resp.StatusCode, string(respBody))
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("chat parse envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return nil, fmt.Errorf("chat: no choices in response")
	}

	var decision BotDecision
	cleanContent := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if err := json.Unmarshal([]byte(cleanContent), &decision); err != nil {
		// Try to extract JSON from within the content
		start := strings.Index(cleanContent, "{")
		end := strings.LastIndex(cleanContent, "}")
		if start != -1 && end != -1 && end > start {
			_ = json.Unmarshal([]byte(cleanContent[start:end+1]), &decision)
		}
		if decision.Reply == "" {
			decision.Reply = cleanContent
			decision.Status = "ongoing"
			decision.Reasoning = "Recovered from raw text"
		}
	}

	if decision.Status != "ongoing" && decision.Status != "ended" {
		decision.Status = "ongoing"
	}

	return &decision, nil
}

// ---------------------------------------------------------------------------
// 5.3 Text-to-Speech (Orpheus)
// ---------------------------------------------------------------------------

var (
	ttsBlockMu    sync.RWMutex
	ttsBlockUntil time.Time // back off for 30s on 429, not 10 minutes
)

// Speak synthesises text using Groq Orpheus TTS and returns the raw WAV bytes.
func (c *GroqClient) Speak(text string) ([]byte, error) {
	return c.SpeakWithContext(context.Background(), text)
}

// SpeakWithContext synthesises text using Groq TTS respecting context cancellation.
// Timeout should be set by the caller — recommended 3500ms for short sentences.
func (c *GroqClient) SpeakWithContext(ctx context.Context, text string) ([]byte, error) {
	// Skip if recently rate-limited
	ttsBlockMu.RLock()
	isBlocked := time.Now().Before(ttsBlockUntil)
	ttsBlockMu.RUnlock()
	if isBlocked {
		return nil, fmt.Errorf("tts rate-limited: backing off")
	}

	ttsModel := os.Getenv("GROQ_TTS_MODEL")
	if ttsModel == "" {
		ttsModel = "playai-tts"
	}
	ttsVoice := os.Getenv("GROQ_TTS_VOICE")
	if ttsVoice == "" {
		ttsVoice = "Fritz-PlayAI"
	}
	payload := map[string]interface{}{
		"model":           ttsModel,
		"voice":           ttsVoice,
		"input":           text,
		"response_format": "wav",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("tts marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groqBaseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts HTTP: %w", err)
	}
	defer resp.Body.Close()

	audioBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tts read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests {
			ttsBlockMu.Lock()
			ttsBlockUntil = time.Now().Add(30 * time.Second) // 30s back-off, not 10 minutes
			ttsBlockMu.Unlock()
			log.Printf("TTS rate limited (429), backing off 30s")
		}
		return nil, fmt.Errorf("tts API error %d: %s", resp.StatusCode, string(audioBytes))
	}

	return audioBytes, nil
}

// SpeakBase64 returns audio as base64-encoded string.
func (c *GroqClient) SpeakBase64(text string) (string, error) {
	return c.SpeakBase64WithContext(context.Background(), text)
}

// SpeakBase64WithContext returns audio as base64 with context cancellation.
func (c *GroqClient) SpeakBase64WithContext(ctx context.Context, text string) (string, error) {
	raw, err := c.SpeakWithContext(ctx, text)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
