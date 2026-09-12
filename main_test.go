package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	if age, year, ok := parseAge("I was born in 1980"); !ok || year != 1980 || age != time.Now().Year()-1980 {
		t.Fatalf("birth year parsing failed: age=%d year=%d ok=%v", age, year, ok)
	}
	if age, _, ok := parseAge("I am sixty five"); !ok || age != 65 {
		t.Fatalf("word age parsing failed: age=%d ok=%v", age, ok)
	}
	if age, _, ok := parseAge("I am in my sixties"); !ok || age != 60 {
		t.Fatalf("decade age parsing failed: age=%d ok=%v", age, ok)
	}
	if _, _, ok := parseAge("I would rather not say"); ok {
		t.Fatal("non-answer should not parse as an age")
	}
}

func TestQualificationFlowAndDispositions(t *testing.T) {
	if defaultBot.MaxSeconds != 90 {
		t.Fatalf("call limit should be 90 seconds, got %d", defaultBot.MaxSeconds)
	}
	s := &Session{Step: "age", StartedAt: time.Now().UTC()}

	first, next := classify(s, "I was born in 1960")
	if next != "done" || s.Age != time.Now().Year()-1960 || len(first.AudioFiles) != 3 || first.AudioFiles[0] != "acknowledge_natural.wav" || first.AudioFiles[1] != "age_confirm.wav" || first.AudioFiles[2] != "age_handoff.wav" || first.Disposition != "RXFER" {
		t.Fatalf("age step failed: result=%+v next=%q age=%d", first, next, s.Age)
	}
	s.Step = "coverage"

	second, next := classify(s, "no")
	if next != "transfer_confirm" || second.Audio != "transfer_offer.wav" {
		t.Fatalf("coverage step failed: result=%+v next=%q", second, next)
	}
	s.Step = next

	third, next := classify(s, "yes")
	if next != "done" || third.Disposition != "RXFER" {
		t.Fatalf("transfer step failed: result=%+v next=%q", third, next)
	}

	dnc, next := classify(s, "Please do not call me again")
	if next != "done" || dnc.Disposition != "DNC" {
		t.Fatalf("DNC disposition failed: result=%+v next=%q", dnc, next)
	}
}

func TestQuestionAnswersDoNotAdvanceFlow(t *testing.T) {
	t.Setenv("AUTOMATED_CALL_DISCLOSURE", "0")
	s := &Session{Step: "age"}
	answer, next := classify(s, "Are you a real person?")
	if next != "age" || !strings.Contains(answer.Text, "Daniel Brooks") || strings.Contains(answer.Text, "automated calling assistant") {
		t.Fatalf("identity answer changed flow: result=%+v next=%q", answer, next)
	}
	name, next := classify(s, "What is your last name?")
	if next != "age" || !strings.Contains(name.Text, "Daniel Brooks") {
		t.Fatalf("last-name answer changed flow: result=%+v next=%q", name, next)
	}
	location, next := classify(s, "Are you in the USA?")
	if next != "age" || location.Audio != "location.wav" {
		t.Fatalf("location answer changed flow: result=%+v next=%q", location, next)
	}
}

func TestSpeechRecognitionVariantsDoNotDisposeCall(t *testing.T) {
	s := &Session{Step: "age"}
	result, next := classify(s, "what teachers do want")
	if next != "age" || result.Disposition != "" || len(result.AudioFiles) != 2 || result.AudioFiles[1] != "information_needed.wav" {
		t.Fatalf("speech-recognition variant was not treated as a details question: result=%+v next=%q", result, next)
	}
	result, next = classify(s, "what details do you need")
	if next != "age" || result.AudioFiles[1] != "information_needed.wav" {
		t.Fatalf("details question was misclassified as identity: result=%+v next=%q", result, next)
	}
}

func TestAutomatedDisclosureCanBeEnabledForFinalBuild(t *testing.T) {
	t.Setenv("AUTOMATED_CALL_DISCLOSURE", "1")
	if !strings.Contains(assistantIdentity("bot_identity"), "automated calling assistant") {
		t.Fatal("final disclosure switch did not restore the required wording")
	}
}

func TestVoiceFolderSelection(t *testing.T) {
	if got := safeVoiceFolder("aura-2-orpheus-en"); got != "aura-2-orpheus-en" {
		t.Fatalf("voice folder changed unexpectedly: %q", got)
	}
	if got := safeVoiceFolder("../escape"); got != "aura-2-orpheus-en" {
		t.Fatalf("voice folder was not pinned to Orpheus: %q", got)
	}
	app := &App{voices: "voices", voiceModel: "aura-2-orpheus-en"}
	result := app.withAudio(classification{Audio: "opening_benefits.wav"})
	if result.Audio != "aura-2-orpheus-en/opening_benefits.wav" {
		t.Fatalf("voice path was not namespaced: %q", result.Audio)
	}
}

func TestRepeatedNonProgressDisposesNI(t *testing.T) {
	s := &Session{Step: "age"}
	for i := 0; i < 2; i++ {
		result, next := classify(s, "What is your favorite movie?")
		if next != "age" || result.Disposition != "" {
			t.Fatalf("unexpected early disposition: result=%+v next=%q", result, next)
		}
	}
	result, next := classify(s, "Can you tell me a joke?")
	if next != "done" || result.Disposition != "NI" || result.Action != "DISPOSE" {
		t.Fatalf("repeated non-progress did not dispose NI: result=%+v next=%q", result, next)
	}
}

func TestNoAnswerDisposition(t *testing.T) {
	app := &App{sessions: map[string]*Session{}, voices: "voices", voiceModel: "aura-2-orpheus-en", noAnswerSeconds: 8}
	s := &Session{ID: "silent", Bot: defaultBot, Step: "age", StartedAt: time.Now().UTC()}
	app.sessions[s.ID] = s
	body, _ := json.Marshal(map[string]string{"session_id": s.ID})
	req := httptest.NewRequest(http.MethodPost, "/api/calls/no-answer", bytes.NewReader(body))
	res := httptest.NewRecorder()
	app.noAnswer(res, req)
	if res.Code != http.StatusOK || !s.Ended || s.Disposition != "NO_ANSWER" {
		t.Fatalf("no-answer endpoint failed: status=%d session=%+v", res.Code, s)
	}
}

func TestAMDMapsMachineToDisposition(t *testing.T) {
	body := bytes.NewBufferString(`{"call_id":"abc","status":"machine","cause":"LONGGREETING"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/vicidial/amd", body)
	res := httptest.NewRecorder()
	(&App{}).vicidialAMD(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("AMD endpoint status=%d", res.Code)
	}
	var response map[string]any
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response["disposition"] != "AMD" || response["dispose"] != true {
		t.Fatalf("unexpected AMD mapping: %+v", response)
	}
}

func TestBusyDisposesBusy(t *testing.T) {
	s := &Session{Step: "age"}
	result, next := classify(s, "I am busy right now")
	if next != "done" || result.Disposition != "BUSY" || result.Action != "DISPOSE" {
		t.Fatalf("busy did not dispose BUSY: result=%+v next=%q", result, next)
	}
	callback, next := classify(s, "Please call me later")
	if next != "done" || callback.Disposition != "CALLBK" {
		t.Fatalf("callback was not kept separate from busy: result=%+v next=%q", callback, next)
	}
}

func TestOutOfRangeAgeDisposesNotEligible(t *testing.T) {
	for _, input := range []string{"I am 49", "I am 81"} {
		s := &Session{Step: "age"}
		result, next := classify(s, input)
		if next != "done" || result.Disposition != "NOT_ELIGIBLE" || result.Action != "DISPOSE" {
			t.Fatalf("out-of-range age was not disposed as not eligible: input=%q result=%+v next=%q", input, result, next)
		}
	}
}

func TestDirectNotInterestedDisposesImmediately(t *testing.T) {
	s := &Session{Step: "age"}
	result, next := classify(s, "I am not interested")
	if next != "done" || result.Disposition != "NI" || result.Action != "DISPOSE" {
		t.Fatalf("direct not-interested did not dispose immediately: result=%+v next=%q", result, next)
	}
}

func TestBusinessContextDisposesBusinessNumber(t *testing.T) {
	for _, input := range []string{"This is a business number", "This is a school", "This is not a home number"} {
		s := &Session{Step: "age"}
		result, next := classify(s, input)
		if next != "done" || result.Disposition != "BUSINESS_NUMBER" || result.Action != "DISPOSE" {
			t.Fatalf("business context was not disposed correctly: input=%q result=%+v next=%q", input, result, next)
		}
	}
}

func TestNotInterestedWithDoNotCallUsesNI(t *testing.T) {
	s := &Session{Step: "age"}
	result, next := classify(s, "I am not interested, do not call again")
	if next != "done" || result.Disposition != "NI" || result.Action != "DISPOSE" {
		t.Fatalf("combined not-interested request did not use NI: result=%+v next=%q", result, next)
	}
}

func TestCoverageKeywordsAdvanceFlow(t *testing.T) {
	s := &Session{Step: "coverage"}
	result, next := classify(s, "I have Medicare")
	if next != "transfer_confirm" || result.Disposition != "" || s.Coverage != "yes" {
		t.Fatalf("coverage keyword was not understood: result=%+v next=%q coverage=%q", result, next, s.Coverage)
	}
}

func TestExplicitHumanRequestUsesRXFER(t *testing.T) {
	s := &Session{Step: "age"}
	result, next := classify(s, "Please connect me to a real person")
	if next != "done" || result.Action != "TRANSFER" || result.Disposition != "RXFER" {
		t.Fatalf("human request did not use RXFER: result=%+v next=%q", result, next)
	}
}

func TestAbusiveLanguageDisposesDNC(t *testing.T) {
	s := &Session{Step: "age"}
	for _, input := range []string{"This is fucking annoying", "sh1t, remove my number"} {
		result, next := classify(s, input)
		if next != "done" || result.Disposition != "DNC" || result.Action != "DNC" {
			t.Fatalf("abusive language did not dispose DNC: input=%q result=%+v next=%q", input, result, next)
		}
	}
}
