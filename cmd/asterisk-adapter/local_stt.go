package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
)

// localSTTClient speaks the tiny loopback protocol implemented by
// scripts/vosk_server.py. The model stays loaded in that process, so calls do
// not pay model-load latency and no audio leaves the host.
type localSTTClient struct {
	conn    net.Conn
	writeMu sync.Mutex
	scanner *bufio.Scanner
}

type sttEvent struct {
	Type       string  `json:"type"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

func connectLocalSTT(address string, sampleRate int, grammar []string) (*localSTTClient, error) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to local Vosk at %s: %w", address, err)
	}
	client := &localSTTClient{conn: conn, scanner: bufio.NewScanner(conn)}
	initPayload := map[string]any{"sample_rate": sampleRate}
	if len(grammar) > 0 {
		initPayload["grammar"] = grammar
	}
	init, _ := json.Marshal(initPayload)
	if _, err := conn.Write(append(init, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize local Vosk stream: %w", err)
	}
	return client, nil
}

func (c *localSTTClient) SetGrammar(phrases []string) error {
	payload, err := json.Marshal(map[string]any{"type": "grammar", "phrases": phrases})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, 0x80000000|uint32(len(payload)))
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err = c.conn.Write(payload)
	return err
}

func (c *localSTTClient) SendPCM(pcm []byte) error {
	if len(pcm) == 0 || len(pcm) > 1<<20 {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, uint32(len(pcm)))
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(pcm)
	return err
}

func (c *localSTTClient) ReadEvent() (sttEvent, error) {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return sttEvent{}, err
		}
		return sttEvent{}, io.EOF
	}
	var event sttEvent
	if err := json.Unmarshal(c.scanner.Bytes(), &event); err != nil {
		return sttEvent{}, err
	}
	return event, nil
}

func (c *localSTTClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// AudioSocket supplies 8 kHz PCM while the small English Vosk model expects
// 16 kHz. Linear interpolation avoids the harsh spectral images produced by
// sample duplication while remaining allocation-light for 20 ms telephony
// frames.
func upsample8kTo16k(input []byte) []byte {
	if len(input) < 2 {
		return nil
	}
	out := make([]byte, len(input)*2)
	for i := 0; i+1 < len(input); i += 2 {
		inSample := int16(binary.LittleEndian.Uint16(input[i : i+2]))
		next := inSample
		if i+3 < len(input) {
			next = int16(binary.LittleEndian.Uint16(input[i+2 : i+4]))
		}
		mid := int16((int32(inSample) + int32(next)) / 2)
		j := i * 2
		binary.LittleEndian.PutUint16(out[j:j+2], uint16(inSample))
		binary.LittleEndian.PutUint16(out[j+2:j+4], uint16(mid))
	}
	return out
}

func grammarForStep(step string) []string {
	switch step {
	case "age":
		return []string{"[unk]", "zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety", "I am", "years old", "born in", "nineteen", "two thousand"}
	case "coverage":
		return []string{"[unk]", "yes", "no", "yeah", "nope", "Medicare", "Medicaid", "insurance", "coverage", "benefits", "I have coverage", "I do not have coverage", "not sure"}
	case "transfer_confirm":
		return []string{"[unk]", "yes", "no", "yeah", "nope", "sure", "okay", "go ahead", "transfer me", "connect me", "not now", "call me later"}
	default:
		return nil
	}
}
