package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAudioSocketFramesRoundTrip(t *testing.T) {
	var wire bytes.Buffer
	payload := []byte{1, 2, 3, 4}
	if err := writeFrame(&wire, frameAudio, payload); err != nil {
		t.Fatal(err)
	}
	frameType, got, err := readFrame(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if frameType != frameAudio || !bytes.Equal(got, payload) {
		t.Fatalf("frame round trip = type %#x payload %v", frameType, got)
	}
}

func TestUpsample8kTo16kDuplicatesSamples(t *testing.T) {
	in := []byte{1, 2, 3, 4}
	want := []byte{1, 2, 2, 3, 3, 4, 3, 4}
	got := upsample8kTo16k(in)
	if !bytes.Equal(got, want) {
		t.Fatalf("upsample = %v, want %v", got, want)
	}
}

func TestVoicePathCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := safeVoicePath(root, "../secret.wav"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	path, err := safeVoicePath(root, "aura-2-orpheus-en/opening.wav")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(root, "aura-2-orpheus-en", "opening.wav") {
		t.Fatalf("unexpected path: %s", path)
	}
}

func TestWAVParserRequiresTelephonyPCM(t *testing.T) {
	path := filepath.Join("..", "..", "voices", "aura-2-orpheus-en", "opening_benefits.wav")
	if _, err := os.Stat(path); err != nil {
		t.Skip("generated voices are not available")
	}
	pcm, err := wavPCM8k(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		t.Fatalf("unexpected PCM payload size: %d", len(pcm))
	}
}
