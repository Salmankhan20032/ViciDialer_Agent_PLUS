// asterisk-adapter bridges Asterisk AudioSocket to a local Vosk streaming STT and
// the local VICIdial bot HTTP API. It deliberately contains no dialplan or
// queue assumptions: callers configure transfer/disposition webhooks for their
// own VICIdial installation.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	frameHangup = 0x00
	frameUUID   = 0x01
	frameAudio  = 0x10 // signed 16-bit little-endian, mono, 8 kHz PCM
)

type config struct {
	listenAddr      string
	browserAddr     string
	botURL          string
	voicesDir       string
	voskAddr        string
	dispositionHook string
	transferHook    string
}

type botReply struct {
	Text        string   `json:"text"`
	AudioFile   string   `json:"audio_file"`
	AudioFiles  []string `json:"audio_files"`
	Action      string   `json:"action"`
	Disposition string   `json:"disposition"`
}

type startResponse struct {
	SessionID string   `json:"session_id"`
	Reply     botReply `json:"reply"`
}

type adapterSession struct {
	config    config
	conn      net.Conn
	writeMu   sync.Mutex
	stt       *localSTTClient
	sessionID string
	callID    string
	turnMu    sync.Mutex
	playMu    sync.Mutex
	playStop  context.CancelFunc
	playID    uint64
}

func main() {
	cfg := config{
		listenAddr:      env("AUDIO_SOCKET_ADDR", ":9019"),
		browserAddr:     env("BROWSER_STT_ADDR", "127.0.0.1:9020"),
		botURL:          strings.TrimRight(env("BOT_URL", "http://127.0.0.1:8080"), "/"),
		voicesDir:       env("VOICES_DIR", "voices"),
		voskAddr:        env("VOSK_ADDR", "127.0.0.1:9021"),
		dispositionHook: os.Getenv("VICIDIAL_DISPOSITION_WEBHOOK_URL"),
		transferHook:    os.Getenv("VICIDIAL_TRANSFER_WEBHOOK_URL"),
	}
	if cfg.browserAddr != "" {
		go serveBrowserSTT(cfg)
	}
	listener, err := net.Listen("tcp", cfg.listenAddr)
	if err != nil {
		log.Fatalf("listen on %s: %v", cfg.listenAddr, err)
	}
	log.Printf("Asterisk AudioSocket adapter listening on %s; bot=%s; STT=local Vosk (%s)", cfg.listenAddr, cfg.botURL, cfg.voskAddr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func() {
			if err := (&adapterSession{config: cfg, conn: conn}).run(); err != nil && !errors.Is(err, io.EOF) {
				log.Printf("AudioSocket session %s: %v", conn.RemoteAddr(), err)
			}
		}()
	}
}

func (s *adapterSession) run() error {
	defer s.conn.Close()
	defer s.stopPlayback()

	frameType, payload, err := readFrame(s.conn)
	if err != nil {
		return err
	}
	if frameType != frameUUID || len(payload) != 16 {
		return fmt.Errorf("expected 16-byte AudioSocket UUID, received frame %#x (%d bytes)", frameType, len(payload))
	}
	s.callID = fmt.Sprintf("%x", payload)
	if err := s.startBot(); err != nil {
		return err
	}
	if err := s.connectLocalSTT(16000); err != nil {
		return err
	}
	defer s.stt.Close()
	go s.readLocalSTT()

	for {
		frameType, payload, err := readFrame(s.conn)
		if err != nil {
			return err
		}
		switch frameType {
		case frameAudio:
			err = s.stt.SendPCM(upsample8kTo16k(payload))
			if err != nil {
				return fmt.Errorf("send audio to local Vosk: %w", err)
			}
		case frameHangup:
			return nil
		}
	}
}

func (s *adapterSession) startBot() error {
	var started startResponse
	if err := postJSON(s.config.botURL+"/api/calls/start", map[string]string{"call_id": s.callID}, &started); err != nil {
		return fmt.Errorf("start bot session: %w", err)
	}
	if started.SessionID == "" {
		return errors.New("bot start response did not include session_id")
	}
	s.sessionID = started.SessionID
	s.handleReply(started.Reply)
	return nil
}

func (s *adapterSession) connectLocalSTT(sampleRate int) error {
	conn, err := connectLocalSTT(s.config.voskAddr, sampleRate, grammarForStep("age"))
	if err != nil {
		return err
	}
	s.stt = conn
	return nil
}

func serveBrowserSTT(cfg config) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(request *http.Request) bool {
			origin := request.Header.Get("Origin")
			if origin == "" {
				// Non-browser health checks and local CLI probes do not send an
				// Origin header. This listener binds to loopback by default.
				host, _, splitErr := net.SplitHostPort(request.Host)
				if splitErr != nil {
					host = request.Host
				}
				return host == "localhost" || host == "127.0.0.1" || host == "[::1]"
			}
			parsed, err := url.Parse(origin)
			if err != nil {
				return false
			}
			host := parsed.Hostname()
			return host == "localhost" || host == "127.0.0.1" || host == "::1"
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/stt", func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		stt, err := connectLocalSTT(cfg.voskAddr, 16000, nil)
		if err != nil {
			_ = connection.WriteJSON(map[string]string{"type": "error", "error": "Could not start local transcription. Start scripts/vosk_server.py first."})
			return
		}
		defer stt.Close()
		if err := connection.WriteJSON(map[string]string{"type": "ready"}); err != nil {
			return
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			defer stt.Close()
			for {
				messageType, audio, err := connection.ReadMessage()
				if err != nil {
					return
				}
				if messageType == websocket.BinaryMessage && len(audio) > 0 {
					if err := stt.SendPCM(audio); err != nil {
						return
					}
				}
			}
		}()

		for {
			event, err := stt.ReadEvent()
			if err != nil {
				return
			}
			if event.Type == "speech_started" {
				if err := connection.WriteJSON(map[string]string{"type": "speech_started"}); err != nil {
					return
				}
				continue
			}
			if event.Type != "result" {
				select {
				case <-done:
					return
				default:
				}
				continue
			}
			transcript := strings.TrimSpace(event.Text)
			if transcript != "" {
				if err := connection.WriteJSON(map[string]string{"type": "transcript", "text": transcript}); err != nil {
					return
				}
			}
		}
	})
	server := &http.Server{Addr: cfg.browserAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("Local Vosk microphone preview listening on %s", cfg.browserAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("browser STT listener: %v", err)
	}
}

func (s *adapterSession) readLocalSTT() {
	for {
		event, err := s.stt.ReadEvent()
		if err != nil {
			return
		}
		if event.Type == "speech_started" {
			s.stopPlayback()
			continue
		}
		if event.Type != "result" {
			continue
		}
		transcript := strings.TrimSpace(event.Text)
		if transcript != "" {
			if event.Confidence > 0 && event.Confidence < 0.25 {
				transcript = "I did not catch that"
			}
			go s.sendTurn(transcript)
		}
	}
}

func (s *adapterSession) sendTurn(text string) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	var response struct {
		Reply   botReply `json:"reply"`
		Session struct {
			Step string `json:"step"`
		} `json:"session"`
	}
	if err := postJSON(s.config.botURL+"/api/calls/turn", map[string]string{"session_id": s.sessionID, "text": text}, &response); err != nil {
		log.Printf("call %s submit transcript: %v", s.callID, err)
		return
	}
	if response.Session.Step != "" {
		if err := s.stt.SetGrammar(grammarForStep(response.Session.Step)); err != nil {
			log.Printf("call %s update Vosk grammar: %v", s.callID, err)
		}
	}
	s.handleReply(response.Reply)
}

func (s *adapterSession) handleReply(reply botReply) {
	played := make(chan struct{})
	files := reply.AudioFiles
	if len(files) == 0 && reply.AudioFile != "" {
		files = []string{reply.AudioFile}
	}
	if len(files) > 0 {
		go func() {
			for _, file := range files {
				if err := s.playWAV(file); err != nil {
					log.Printf("call %s play %s: %v", s.callID, file, err)
					break
				}
			}
			close(played)
		}()
	} else {
		close(played)
	}
	if reply.Disposition == "" && reply.Action != "TRANSFER" {
		return
	}
	// Let the caller hear the confirmation or closing statement before a
	// customer-specific webhook changes the live Asterisk channel.
	go func() {
		<-played
		if reply.Disposition != "" {
			s.notify(s.config.dispositionHook, map[string]string{"call_id": s.callID, "session_id": s.sessionID, "disposition": reply.Disposition, "action": reply.Action})
		}
		if reply.Action == "TRANSFER" {
			s.notify(s.config.transferHook, map[string]string{"call_id": s.callID, "session_id": s.sessionID, "disposition": reply.Disposition})
		}
	}()
}

func (s *adapterSession) notify(destination string, payload map[string]string) {
	if destination == "" {
		return
	}
	go func() {
		if err := postJSON(destination, payload, nil); err != nil {
			log.Printf("call %s notify %s: %v", s.callID, destination, err)
		}
	}()
}

func (s *adapterSession) playWAV(audioFile string) error {
	path, err := safeVoicePath(s.config.voicesDir, audioFile)
	if err != nil {
		return err
	}
	pcm, err := wavPCM8k(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.playMu.Lock()
	if s.playStop != nil {
		s.playStop()
	}
	s.playID++
	playID := s.playID
	s.playStop = cancel
	s.playMu.Unlock()
	defer func() {
		s.playMu.Lock()
		if s.playID == playID {
			s.playStop = nil
		}
		s.playMu.Unlock()
	}()

	const bytesPer20ms = 320 // 8,000 samples/s × 2 bytes/sample × 0.02 s
	for offset := 0; offset < len(pcm); offset += bytesPer20ms {
		end := offset + bytesPer20ms
		if end > len(pcm) {
			end = len(pcm)
		}
		if offset > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(20 * time.Millisecond):
			}
		}
		if err := s.writeFrame(frameAudio, pcm[offset:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *adapterSession) stopPlayback() {
	s.playMu.Lock()
	if s.playStop != nil {
		s.playStop()
		s.playStop = nil
	}
	s.playID++
	s.playMu.Unlock()
}

func (s *adapterSession) writeFrame(frameType byte, payload []byte) error {
	if len(payload) > 65535 {
		return errors.New("AudioSocket frame exceeds 65535 bytes")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeFrame(s.conn, frameType, payload)
}

func readFrame(reader io.Reader) (byte, []byte, error) {
	var header [3]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := int(binary.BigEndian.Uint16(header[1:]))
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func writeFrame(writer io.Writer, frameType byte, payload []byte) error {
	if len(payload) > 65535 {
		return errors.New("AudioSocket frame exceeds 65535 bytes")
	}
	header := []byte{frameType, 0, 0}
	binary.BigEndian.PutUint16(header[1:], uint16(len(payload)))
	if _, err := writer.Write(header); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func safeVoicePath(root, audioFile string) (string, error) {
	if audioFile == "" || filepath.IsAbs(audioFile) {
		return "", errors.New("invalid voice path")
	}
	for _, part := range strings.Split(audioFile, "/") {
		if part == "" || part == "." || part == ".." || filepath.Base(part) != part {
			return "", errors.New("invalid voice path")
		}
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(rootAbs, audioFile))
	if err != nil || !strings.HasPrefix(path, rootAbs+string(os.PathSeparator)) {
		return "", errors.New("voice path escapes voice directory")
	}
	return path, nil
}

func wavPCM8k(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var channels, sampleRate, bitsPerSample uint32
	var pcm []byte
	for offset := 12; offset+8 <= len(data); {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if chunkSize < 0 {
			return nil, errors.New("invalid WAV chunk")
		}
		// Some telephony WAV generators leave the RIFF data length at a
		// placeholder larger than the actual stream. The PCM is still valid;
		// for the final data chunk, safely use the bytes that are present.
		if offset+chunkSize > len(data) {
			if chunkID != "data" {
				return nil, errors.New("invalid WAV chunk")
			}
			chunkSize = len(data) - offset
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 || binary.LittleEndian.Uint16(data[offset:offset+2]) != 1 {
				return nil, errors.New("voice WAV must be PCM")
			}
			channels = uint32(binary.LittleEndian.Uint16(data[offset+2 : offset+4]))
			sampleRate = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			bitsPerSample = uint32(binary.LittleEndian.Uint16(data[offset+14 : offset+16]))
		case "data":
			pcm = append([]byte(nil), data[offset:offset+chunkSize]...)
		}
		offset += chunkSize
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if channels != 1 || sampleRate != 8000 || bitsPerSample != 16 || len(pcm) == 0 {
		return nil, fmt.Errorf("voice WAV must be 16-bit mono 8 kHz PCM (got channels=%d rate=%d bits=%d)", channels, sampleRate, bitsPerSample)
	}
	return pcm, nil
}

func postJSON(destination string, body any, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, destination, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(message)))
	}
	if target != nil {
		return json.NewDecoder(response.Body).Decode(target)
	}
	return nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	var value int
	if _, err := fmt.Sscanf(os.Getenv(name), "%d", &value); err != nil || value < 10 || value > 2000 {
		return fallback
	}
	return value
}
