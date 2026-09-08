package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"strconv"
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
	sysPrompt      string
	thinkingBudget int
	ws             *websocket.Conn
	writeMu        sync.Mutex
	isClosed       bool
	closedMu       sync.Mutex
	done           chan struct{}

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
	thinkingBudget := 256
	if tbEnv := os.Getenv("GEMINI_LIVE_THINKING_BUDGET"); tbEnv != "" {
		if tb, err := strconv.Atoi(tbEnv); err == nil {
			thinkingBudget = tb
		}
	}
	return &GeminiLiveSession{
		apiKey:         apiKey,
		model:          model,
		voice:          voice,
		sysPrompt:      sysPrompt,
		thinkingBudget: thinkingBudget,
		done:           make(chan struct{}),
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
					ThinkingBudget: s.thinkingBudget,
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
										"description": "VICIdial status code: UNDRAG (Under Age Disqualified - age under 50), OVERAG (Over Age Disqualified - age over 80), SALE (Sale/Interest Confirmed), NI (Not Interested), CALLBK (Call Back Requested), DNC (Do Not Call), DEC (Declined), LB (Language Barrier), A (Answering Machine/Voicemail), XFER (Transfer to Licensed Agent), CxHANG (Prospect Hung Up)",
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

// SendOpeningPrompt triggers Gemini to speak the warm opening greeting as the persona.
func (s *GeminiLiveSession) SendOpeningPrompt(persona AgentPersona) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.isClosed || s.ws == nil {
		return fmt.Errorf("session closed")
	}

	if persona.FirstName == "" {
		persona.FirstName = "Sarah"
	}
	if persona.LastName == "" {
		persona.LastName = "Miller"
	}
	if persona.FullName == "" {
		persona.FullName = persona.FirstName + " " + persona.LastName
	}
	if persona.Company == "" {
		persona.Company = "Senior Benefit Services"
	}

	greetingText := fmt.Sprintf(
		`[CALL CONNECTED — The prospect just picked up the phone. In character as %s, warmly speak your opening line right now: "Hi, this is %s. I’m calling about new benefit options for your age group to see if you qualify. Can I ask how old you are?"]`,
		persona.FullName, persona.FirstName,
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
func BuildGeminiLiveSystemPrompt(campaignScript string, persona AgentPersona) string {
	if persona.FirstName == "" {
		persona.FirstName = "Sarah"
	}
	if persona.LastName == "" {
		persona.LastName = "Miller"
	}
	if persona.FullName == "" {
		persona.FullName = persona.FirstName + " " + persona.LastName
	}
	if persona.Company == "" {
		persona.Company = "Senior Benefit Services"
	}

	var sb strings.Builder
	for _, d := range Dispositions {
		sb.WriteString(fmt.Sprintf("- %s (%s): %s\n", d.Code, d.Label, d.Description))
	}
	dispositionTable := sb.String()

	return fmt.Sprintf(`YOUR IDENTITY & PERSONA:
- Your Name: %s (First Name: %s, Last Name: %s)
- Your Company: %s
- Your Role: Warm, respectful, and professional customer care representative calling to see if prospects qualify for state benefit and coverage options designed to help families with final expenses and costs at the time of death.
- Active Channel: Real-time outbound live phone call.

CAMPAIGN SCRIPT & FLOW:
%s

VOICE CONVERSATION GUIDELINES:
1. NATURAL SPOKEN ENGLISH ONLY:
   - Speak only in clear, fluent, natural American English with a warm and reassuring telephone demeanor.
   - If the prospect speaks another language or mumbles inaudibly, say politely: "I'm sorry, I couldn't quite hear that. Could you please repeat that in English?"

2. ONE QUESTION AT A TIME — CONVERSATIONAL PACING (CONCISE):
   - Keep every response concise (1 short sentence + 1 simple question maximum).
   - Never stack multiple questions. Never deliver long monologues or paragraphs.
   - When you ask a question, stop speaking immediately and listen for the prospect's answer.

3. MANDATORY INTERRUPTION & ASIDE QUESTION HANDLING (CRITICAL — NEVER SKIP):
   - The prospect may interrupt you or ask unexpected side questions at ANY point during the conversation.
   - YOU MUST ANSWER THEIR SPECIFIC QUESTION IMMEDIATELY AND DIRECTLY FIRST. NEVER SKIP, DODGE, OR IGNORE WHAT THEY ASK!
   - Specific questions you MUST answer directly:
     * "What's your last name?" / "What is your last name?":
       Answer directly: "My last name is %s! %s with %s."
     * "Who are you?" / "Who is this?":
       Answer directly: "I'm %s with %s."
     * "What company is this?" / "Who do you work for?":
       Answer directly: "I'm calling with %s."
     * "Where are you located?" / "Where are you calling from?":
       Answer directly: "We are an American senior benefit outreach service calling on behalf of state final expense programs."
     * "Are you a real person?" / "Is this an AI / robot?":
       Answer directly: "Yes, I'm a live representative right here with you! My name is %s."
   - IMMEDIATELY after directly answering their side question, smoothly steer back to the current step in the conversation in the same turn without skipping a beat (for example: "My last name is %s! %s with %s. May I ask how old you are?").

4. SCRIPT FLOW & ACTIONS:
   - Step 1 (Age Qualification — Target Age Group is 50 to 80):
     * The prospect may answer with their direct age (e.g. "I'm 65") OR with their Date of Birth / Birth Year (e.g. "I was born on 31st February 1965", "July 4th, 1960", or "1965"):
       1. DATE OF BIRTH & CALENDAR VERIFICATION (CRITICAL — THINK & VALIDATE DATES CAREFULLY):
          - If the prospect gives a Date of Birth, FIRST CHECK IF THE CALENDAR DATE ACTUALLY EXISTS IN REAL LIFE!
          - February NEVER has 30 or 31 days (February only has 28 days, or 29 days in leap years). There is NO such date as February 30th or February 31st!
          - April, June, September, and November only have 30 days. April 31st, June 31st, September 31st, and November 31st DO NOT EXIST!
          - Days cannot exceed 31, and months cannot exceed 12.
          - IF THE PROSPECT GIVES AN IMPOSSIBLE, FAKE, OR NONSENSE DATE (such as "31st February 1965", "February 30th", "April 31st"):
            DO NOT ACCEPT IT! DO NOT CONFIRM IT! DO NOT PROCEED TO STEP 2!
            You MUST politely and warmly challenge it and ask for their real age:
            "Wait a moment, February only has 28 days! Could you please tell me your actual date of birth or your current age?"
            Stop speaking immediately and wait for their clarification.
       2. CURRENT AGE CALCULATION (CURRENT CALENDAR YEAR IS 2026):
          - Calculate: Age = 2026 - Birth Year.
          - Example: Born in 1965 = 61 years old (50 to 80 -> QUALIFIED).
          - Example: Born in 1985 = 41 years old (Under 50 -> DISQUALIFIED).
          - Example: Born in 1938 = 88 years old (Over 80 -> DISQUALIFIED).

     * If prospect's verified age is between 50 and 80 years old (50 to 80 inclusive, e.g. 52, 65, 78, 80):
       Say: "Perfect, thank you! And are you currently receiving any type of coverage or benefits that would help your family with expenses at the time of death?"
       Wait for their answer.
     * If prospect's verified age is UNDER 50 (e.g. 18 to 49 years old, or "I'm 24", "I'm 40", born in 1985):
       YOU MUST CLEARLY AND POLITELY TELL THEM OUT LOUD:
       "Thank you for letting me know. Unfortunately, this specific program is specifically designed for seniors between the ages of 50 and 80, so this program is not for you at this time. Thank you so much for your time, and have a wonderful day!"
       Immediately invoke tool: set_disposition(status="UNDRAG", notes="Customer age is under 50 (disqualified)") and STOP speaking. Do NOT ask any further questions.
     * If prospect's verified age is OVER 80 (e.g. 81+ years old, or "I'm 85", "I'm 92", born in 1940):
       YOU MUST CLEARLY AND POLITELY TELL THEM OUT LOUD:
       "Thank you for letting me know. Unfortunately, this specific program is specifically designed for seniors between the ages of 50 and 80, so this program is not for you at this time. Thank you so much for your time, and have a wonderful day!"
       Immediately invoke tool: set_disposition(status="OVERAG", notes="Customer age is over 80 (disqualified)") and STOP speaking. Do NOT ask any further questions.

   - Step 2 (Coverage Check):
     * Regardless of whether they currently have coverage or not, say:
       "The reason I’m asking is that there are coverage options designed to help families with those costs, and a licensed specialist can check what you may be eligible for. I can transfer you now so they can go over the details with you. Is that okay?"
       Wait for their reply.

   - Step 3 (Transfer Initiation & Check for Questions):
     * If the prospect agrees to the transfer ("Yes", "Sure", "Okay", "Go ahead", "Yeah"):
       Say: "Great, I'll transfer you now! Before I connect you, do you have any other questions for me?"
       DO NOT invoke set_disposition yet! Stop speaking and wait for their reply.
     * If the prospect declines the transfer ("No thanks", "Not interested", "No"):
       Say: "No problem at all. Thank you for your time and have a great day!"
       Invoke tool: set_disposition(status="NI", notes="Prospect declined transfer") and conclude.

   - Step 4 (Answer Questions & Complete Transfer):
     * If the prospect asks any question (e.g. "Is this free?", "What does it cost?", "Who will I speak with?"):
       Answer their question warmly and concisely (1-2 sentences), then immediately conclude:
       "Thank you for your time, transferring you now, please hold one moment!"
       Invoke tool: set_disposition(status="XFER", notes="Answered question and transferred to specialist") and conclude.
     * If the prospect says they have no questions ("No", "Nope", "No questions", "I'm good", "All set"):
       Say: "Perfect, thank you for your time! Transferring you now, please hold one moment."
       Invoke tool: set_disposition(status="XFER", notes="Transferred to licensed specialist") and conclude.
     * If the prospect changes their mind:
       Say: "No problem at all. Thank you for your time and have a great day!"
       Invoke tool: set_disposition(status="NI", notes="Prospect changed mind") and conclude.
     * If prospect requests callback:
       Say: "Certainly, we will follow up with you at a better time. Have a great day!"
       Invoke tool: set_disposition(status="CALLBK", notes="Callback requested") and conclude.
     * If prospect says do not call / remove from list:
       Say: "I will put you on our do not call list immediately. Have a good day."
       Invoke tool: set_disposition(status="DNC", notes="DNC requested") and conclude.
     * If prospect is an answering machine / voicemail:
       Invoke tool: set_disposition(status="A", notes="Voicemail detected") and disconnect immediately.

5. DISPOSITION TRACKING:
   Whenever a call conclusion or outcome is decided, invoke the tool 'set_disposition' immediately:
%s

Always stay in character as %s. Be polite, concise, attentive, and helpful.`,
		persona.FullName, persona.FirstName, persona.LastName,
		persona.Company,
		campaignScript,
		persona.LastName, persona.FullName, persona.Company,
		persona.FullName, persona.Company,
		persona.Company,
		persona.FullName,
		persona.LastName, persona.FullName, persona.Company,
		dispositionTable,
		persona.FullName,
	)
}
