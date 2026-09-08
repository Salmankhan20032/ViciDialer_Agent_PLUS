package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ---------------------------------------------------------------------------
// Gemini Multimodal Live API Protocol Structs
// ---------------------------------------------------------------------------

// GeminiLiveSetupMessage is the initial handshake payload sent to Gemini Live.
type GeminiLiveSetupMessage struct {
	Setup GeminiLiveSetup `json:"setup"`
}

type GeminiLiveSetup struct {
	Model             string               `json:"model"`
	GenerationConfig         GeminiLiveGenConfig  `json:"generationConfig"`
	SystemInstruction        GeminiLiveContent    `json:"systemInstruction"`
	Tools                    []GeminiLiveTool     `json:"tools,omitempty"`
	InputAudioTranscription  *struct{}            `json:"inputAudioTranscription,omitempty"`
	OutputAudioTranscription *struct{}            `json:"outputAudioTranscription,omitempty"`
}

type GeminiLiveGenConfig struct {
	ResponseModalities []string                  `json:"responseModalities"`
	SpeechConfig       GeminiLiveSpeechConf      `json:"speechConfig"`
	ThinkingConfig     *GeminiLiveThinkingConfig `json:"thinkingConfig,omitempty"`
}

type GeminiLiveThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type GeminiLiveSpeechConf struct {
	VoiceConfig GeminiLiveVoiceConf `json:"voiceConfig"`
}

type GeminiLiveVoiceConf struct {
	PrebuiltVoiceConfig GeminiLivePrebuiltVoice `json:"prebuiltVoiceConfig"`
}

type GeminiLivePrebuiltVoice struct {
	VoiceName string `json:"voiceName"` // "Aoede", "Puck", "Charon", "Kore", "Fenrir"
}

type GeminiLiveContent struct {
	Parts []GeminiLivePart `json:"parts"`
}

type GeminiLivePart struct {
	Text       string          `json:"text,omitempty"`
	InlineData *GeminiLiveBlob `json:"inlineData,omitempty"`
}

type GeminiLiveBlob struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // base64 encoded PCM
}

type GeminiLiveTool struct {
	FunctionDeclarations []GeminiLiveFuncDecl `json:"functionDeclarations"`
}

type GeminiLiveFuncDecl struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

// Client Outbound: Audio PCM Stream
type GeminiLiveRealtimeInput struct {
	RealtimeInput GeminiLiveMediaChunks `json:"realtimeInput"`
}

type GeminiLiveMediaChunks struct {
	MediaChunks []GeminiLiveBlob `json:"mediaChunks"`
}

// Client Outbound: Initial kickoff prompt
type GeminiLiveClientContent struct {
	ClientContent GeminiLiveTurns `json:"clientContent"`
}

type GeminiLiveTurns struct {
	Turns        []GeminiLiveTurn `json:"turns"`
	TurnComplete bool             `json:"turnComplete"`
}

type GeminiLiveTurn struct {
	Role  string           `json:"role"`
	Parts []GeminiLivePart `json:"parts"`
}

// Client Outbound: Tool Response
type GeminiLiveToolResponseMsg struct {
	ToolResponse GeminiLiveToolResponse `json:"toolResponse"`
}

type GeminiLiveToolResponse struct {
	FunctionResponses []GeminiLiveFuncResponse `json:"functionResponses"`
}

type GeminiLiveFuncResponse struct {
	Response map[string]interface{} `json:"response"`
	ID       string                 `json:"id"`
}

// Server Inbound Message
type GeminiLiveServerMessage struct {
	SetupComplete *struct{}                `json:"setupComplete,omitempty"`
	ServerContent *GeminiLiveServerContent `json:"serverContent,omitempty"`
	ToolCall      *GeminiLiveToolCall      `json:"toolCall,omitempty"`
}

type GeminiLiveServerContent struct {
	ModelTurn           *GeminiLiveContent    `json:"modelTurn,omitempty"`
	Interrupted         bool                  `json:"interrupted,omitempty"`
	TurnComplete        bool                  `json:"turnComplete,omitempty"`
	InputTranscription  *GeminiLiveTranscript `json:"inputTranscription,omitempty"`
	OutputTranscription *GeminiLiveTranscript `json:"outputTranscription,omitempty"`
}

type GeminiLiveTranscript struct {
	Text string `json:"text"`
}

type GeminiLiveToolCall struct {
	FunctionCalls []GeminiLiveFuncCall `json:"functionCalls"`
}

type GeminiLiveFuncCall struct {
	ID   string                 `json:"id"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// ---------------------------------------------------------------------------
// Gemini Live Bridge Session
// ---------------------------------------------------------------------------

// GeminiLiveSession manages a single live duplex call directly with Google's Gemini Live API.
type GeminiLiveSession struct {
	apiKey      string
	model       string
	voice       string
	sysPrompt   string
	ws          *websocket.Conn
	writeMu     sync.Mutex
	isClosed    bool
	closedMu    sync.Mutex
	done        chan struct{}

	// Callbacks
	OnAudio        func(pcm24kBase64 string)
	OnTranscript   func(role string, text string)
	OnInterrupted  func()
	OnTurnComplete func()
	OnDisposition  func(code string, notes string)
	OnError        func(err error)
}

// NewGeminiLiveSession creates a new unstarted session.
func NewGeminiLiveSession(apiKey, model, voice, sysPrompt string) *GeminiLiveSession {
	if model == "" {
		model = "models/gemini-2.5-flash-native-audio-latest"
	}
	if voice == "" {
		voice = "Aoede"
	}
	return &GeminiLiveSession{
		apiKey:    apiKey,
		model:     model,
		voice:     voice,
		sysPrompt: sysPrompt,
		done:      make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection to Gemini Live and completes handshake.
func (s *GeminiLiveSession) Connect(ctx context.Context) error {
	if s.apiKey == "" {
		return fmt.Errorf("GEMINI_API_KEY is not set. Please add GEMINI_API_KEY to your .env file")
	}

	wsURL := fmt.Sprintf(
		"wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1alpha.GenerativeService.BidiGenerateContent?key=%s",
		url.QueryEscape(s.apiKey),
	)

	dialer := websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second

	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("gemini live dial error (status %d): %v — %s", resp.StatusCode, err, string(body))
		}
		return fmt.Errorf("gemini live dial error: %w", err)
	}

	s.ws = conn

	// Send Setup message
	setupMsg := GeminiLiveSetupMessage{
		Setup: GeminiLiveSetup{
			Model: s.model,
			GenerationConfig: GeminiLiveGenConfig{
				ResponseModalities: []string{"AUDIO"},
				SpeechConfig: GeminiLiveSpeechConf{
					VoiceConfig: GeminiLiveVoiceConf{
						PrebuiltVoiceConfig: GeminiLivePrebuiltVoice{
							VoiceName: s.voice,
						},
					},
				},
				ThinkingConfig: &GeminiLiveThinkingConfig{
					ThinkingBudget: 0,
				},
			},
			SystemInstruction: GeminiLiveContent{
				Parts: []GeminiLivePart{
					{Text: s.sysPrompt},
				},
			},
			Tools: []GeminiLiveTool{
				{
					FunctionDeclarations: []GeminiLiveFuncDecl{
						{
							Name:        "set_disposition",
							Description: "Updates the VICIdial call disposition code and summary notes in real-time when the outcome is determined or the call reaches a conclusion.",
							Parameters: map[string]interface{}{
								"type": "OBJECT",
								"properties": map[string]interface{}{
									"status": map[string]interface{}{
										"type": "STRING",
										"description": "VICIdial status code: SALE (Sale/Interest Confirmed), NI (Not Interested), CALLBK (Call Back Requested), DNC (Do Not Call), DEC (Declined), LB (Language Barrier), A (Answering Machine/Voicemail), XFER (Transfer to Licensed Agent), CxHANG (Prospect Hung Up)",
									},
									"notes": map[string]interface{}{
										"type":        "STRING",
										"description": "Brief summary notes for VICIdial agent lead sheet",
									},
								},
								"required": []string{"status"},
							},
						},
					},
				},
			},
			InputAudioTranscription:  &struct{}{},
			OutputAudioTranscription: &struct{}{},
		},
	}

	s.writeMu.Lock()
	err = s.ws.WriteJSON(setupMsg)
	s.writeMu.Unlock()
	if err != nil {
		s.ws.Close()
		return fmt.Errorf("failed to send setup message to gemini: %w", err)
	}

	// Read first message — expect setupComplete
	var setupResp GeminiLiveServerMessage
	err = s.ws.ReadJSON(&setupResp)
	if err != nil {
		s.ws.Close()
		return fmt.Errorf("failed to receive setup response from gemini: %w", err)
	}
	if setupResp.SetupComplete == nil {
		log.Printf("⚠️ Gemini setup response did not contain setupComplete: %+v", setupResp)
	} else {
		log.Printf("✅ Gemini Live setupComplete received (voice: %s, model: %s)", s.voice, s.model)
	}

	// Start reading loop in background
	go s.readLoop()

	return nil
}

// SendOpeningPrompt triggers Gemini to speak the warm opening greeting as agentName.
func (s *GeminiLiveSession) SendOpeningPrompt(agentName string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed || s.ws == nil {
		return fmt.Errorf("session closed")
	}

	if agentName == "" {
		agentName = "Sarah"
	}

	greetingText := fmt.Sprintf(
		`[CALL CONNECTED — The prospect just picked up the phone. In character as %s, warmly speak your opening line right now: "Hi, this is %s. I’m calling about new benefit options for your age group to see if you qualify. Can I ask how old you are?"]`,
		agentName, agentName,
	)

	kickoff := GeminiLiveClientContent{
		ClientContent: GeminiLiveTurns{
			Turns: []GeminiLiveTurn{
				{
					Role: "user",
					Parts: []GeminiLivePart{
						{
							Text: greetingText,
						},
					},
				},
			},
			TurnComplete: true,
		},
	}
	return s.ws.WriteJSON(kickoff)
}

// SendPCMChunk streams raw 16kHz PCM audio from the user's mic to Gemini Live.
func (s *GeminiLiveSession) SendPCMChunk(pcm16kBase64 string) error {
	s.closedMu.Lock()
	closed := s.isClosed
	s.closedMu.Unlock()
	if closed || s.ws == nil {
		return fmt.Errorf("session closed")
	}

	input := GeminiLiveRealtimeInput{
		RealtimeInput: GeminiLiveMediaChunks{
			MediaChunks: []GeminiLiveBlob{
				{
					MimeType: "audio/pcm;rate=16000",
					Data:     pcm16kBase64,
				},
			},
		},
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.ws.WriteJSON(input)
}

// sendToolResponse acknowledges a tool call back to Gemini Live.
func (s *GeminiLiveSession) sendToolResponse(callID string, output map[string]interface{}) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed || s.ws == nil {
		return fmt.Errorf("session closed")
	}

	resp := GeminiLiveToolResponseMsg{
		ToolResponse: GeminiLiveToolResponse{
			FunctionResponses: []GeminiLiveFuncResponse{
				{
					ID:       callID,
					Response: map[string]interface{}{"output": output},
				},
			},
		},
	}
	return s.ws.WriteJSON(resp)
}

// readLoop continuously consumes messages from Gemini Live WS and routes to callbacks.
func (s *GeminiLiveSession) readLoop() {
	defer func() {
		s.Close()
	}()

	for {
		s.closedMu.Lock()
		closed := s.isClosed
		s.closedMu.Unlock()
		if closed {
			return
		}

		_, rawBytes, err := s.ws.ReadMessage()
		if err != nil {
			if !closed && s.OnError != nil {
				s.OnError(err)
			}
			return
		}

		var msg GeminiLiveServerMessage
		if err := json.Unmarshal(rawBytes, &msg); err != nil {
			log.Printf("⚠️ Gemini Live json unmarshal error: %v", err)
			continue
		}

		// 1. Tool Call handling (e.g. set_disposition)
		if msg.ToolCall != nil && len(msg.ToolCall.FunctionCalls) > 0 {
			for _, fc := range msg.ToolCall.FunctionCalls {
				if fc.Name == "set_disposition" {
					status, _ := fc.Args["status"].(string)
					notes, _ := fc.Args["notes"].(string)
					log.Printf("🎯 Gemini Live ToolCall: set_disposition(status=%s, notes=%s)", status, notes)
					if s.OnDisposition != nil {
						s.OnDisposition(status, notes)
					}
					// Acknowledge back to Gemini
					_ = s.sendToolResponse(fc.ID, map[string]interface{}{
						"success": true,
						"status":  status,
					})
				} else {
					_ = s.sendToolResponse(fc.ID, map[string]interface{}{
						"status": "unsupported",
					})
				}
			}
		}

		// 2. Server Content handling (Audio, Transcripts, Interruption, Turn Complete)
		if msg.ServerContent != nil {
			// User speech input transcription (live from Gemini)
			if msg.ServerContent.InputTranscription != nil && msg.ServerContent.InputTranscription.Text != "" {
				log.Printf("[Gemini Live] 👤 User transcript: %s", msg.ServerContent.InputTranscription.Text)
				if s.OnTranscript != nil {
					s.OnTranscript("customer", msg.ServerContent.InputTranscription.Text)
				}
			}

			// Bot speech output transcription (live from Gemini)
			if msg.ServerContent.OutputTranscription != nil && msg.ServerContent.OutputTranscription.Text != "" {
				log.Printf("[Gemini Live] 💬 Bot transcript: %s", msg.ServerContent.OutputTranscription.Text)
				if s.OnTranscript != nil {
					s.OnTranscript("bot", msg.ServerContent.OutputTranscription.Text)
				}
			}

			// Barge-in interruption detected server-side by Gemini's voice model
			if msg.ServerContent.Interrupted {
				log.Printf("⚡ Gemini Live detected user barge-in interruption")
				if s.OnInterrupted != nil {
					s.OnInterrupted()
				}
			}

			// Model Turn chunks (Audio PCM 24kHz)
			if msg.ServerContent.ModelTurn != nil {
				for _, part := range msg.ServerContent.ModelTurn.Parts {
					if part.InlineData != nil && len(part.InlineData.Data) > 0 {
						if s.OnAudio != nil {
							s.OnAudio(part.InlineData.Data)
						}
					}
				}
			}

			// Turn complete
			if msg.ServerContent.TurnComplete {
				log.Printf("[Gemini Live] ✅ Bot turn complete")
				if s.OnTurnComplete != nil {
					s.OnTurnComplete()
				}
			}
		}
	}
}

// Close cleanly terminates the Gemini Live WebSocket session.
func (s *GeminiLiveSession) Close() {
	s.closedMu.Lock()
	if s.isClosed {
		s.closedMu.Unlock()
		return
	}
	s.isClosed = true
	s.closedMu.Unlock()

	close(s.done)
	if s.ws != nil {
		_ = s.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "call ended"))
		_ = s.ws.Close()
	}
}

// BuildGeminiLiveSystemPrompt formats the prompt tailored for real-time speech conversation.
func BuildGeminiLiveSystemPrompt(campaignScript string, agentName string) string {
	if agentName == "" {
		agentName = "Sarah"
	}
	var sb strings.Builder
	for _, d := range Dispositions {
		sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", d.Code, d.Label, d.Description))
	}
	dispositionTable := sb.String()

	return fmt.Sprintf(`You are %s, a warm, professional, and respectful representative calling to see if prospects qualify for new state benefit and coverage options designed to help families with final expenses and costs at the time of death. You are speaking in an active, real-time live outbound phone call.

CAMPAIGN SCRIPT & FLOW:
%s

VOICE CONVERSATION GUIDELINES:
1. NATURAL SPOKEN ENGLISH ONLY:
   - Speak only in clear, fluent, natural English with a warm and reassuring telephone demeanor.
   - If the prospect speaks another language or mumbles inaudibly, say politely: "I'm sorry, I couldn't quite hear that. Could you please repeat that in English?"

2. ONE QUESTION AT A TIME — STRICT CONVERSATIONAL PACING (ULTRA-CONCISE):
   - Keep every response under 15-20 words: exactly 1 short sentence + 1 simple question maximum.
   - Never stack multiple questions. Never deliver long monologues or paragraphs. This keeps speech latency ultra-fast and saves token bandwidth.
   - When you ask a question, stop speaking immediately and wait naturally for the prospect's reply.

3. SCRIPT FLOW & ACTIONS:
   - Step 1 (Age Qualification):
     * If prospect states they are between 50 and 80 years old: say "Perfect, thank you. And are you currently receiving any type of coverage or benefits that would help your family with expenses at the time of death?"
     * If prospect is under 50 or over 80: say "Got it. Unfortunately this program is specifically for ages 50 to 80. Thank you for your time and have a wonderful day!" -> invoke set_disposition(status="NI", notes="Age not qualified") and disconnect.
   - Step 2 (Coverage Check):
     * Regardless of whether they currently have coverage or not, say: "The reason I’m asking is that there are coverage options designed to help families with those costs, and a licensed specialist can check what you may be eligible for. I can transfer you now so they can go over the details with you. Is that okay?"
   - Step 3 (Transfer Initiation & Questions Check):
     * When the prospect agrees to the transfer ("Yes", "Sure", "Okay", "Go ahead"):
       Say: "Great, I'll transfer you now! Before I connect you, do you have any other questions for me?"
       DO NOT invoke set_disposition yet! Stop speaking and wait for their reply.
     * If prospect declines the transfer ("No thanks", "Not interested"):
       Say: "No problem at all. Thank you for your time and have a great day!" -> invoke set_disposition(status="NI", notes="Prospect declined transfer") and conclude.
   - Step 4 (Answer Questions & Complete Transfer):
     * If the prospect asks any question (e.g. "Is this free?", "What company?", "How much does it cost?", "Who will I speak with?"):
       Answer their question warmly and concisely (1-2 sentences), then immediately conclude:
       "Thank you for your time, transferring you now, please hold one moment!" -> invoke set_disposition(status="XFER", notes="Answered question and transferred to specialist")
     * If the prospect says they have no other questions ("No", "Nope", "No questions", "I'm good", "All set"):
       Say: "Perfect, thank you for your time! Transferring you now, please hold one moment." -> invoke set_disposition(status="XFER", notes="Transferred to licensed specialist")
     * If the prospect changes their mind:
       Say: "No problem at all. Thank you for your time and have a great day!" -> invoke set_disposition(status="NI", notes="Prospect changed mind")
     * If prospect requests callback: -> invoke set_disposition(status="CALLBK", notes="Callback requested")
     * If prospect says do not call / remove from list: -> invoke set_disposition(status="DNC", notes="DNC requested")
     * If prospect is an answering machine / voicemail: -> invoke set_disposition(status="A", notes="Voicemail detected") and disconnect.

4. DISPOSITION TRACKING:
   Whenever a call conclusion or outcome is decided, invoke the tool 'set_disposition' immediately:
%s

Stay in character as %s at all times. Be polite, concise, and helpful.`,
		agentName,
		campaignScript,
		dispositionTable,
		agentName,
	)
}
