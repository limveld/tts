package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/polly"
)

// fakePolly stands in for the AWS Polly client: it records the request and returns
// canned MP3 bytes, so tests never call AWS.
type fakePolly struct {
	audio []byte

	mu     sync.Mutex
	inputs []*polly.SynthesizeSpeechInput
}

func (f *fakePolly) SynthesizeSpeech(_ context.Context, in *polly.SynthesizeSpeechInput, _ ...func(*polly.Options)) (*polly.SynthesizeSpeechOutput, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()
	return &polly.SynthesizeSpeechOutput{AudioStream: io.NopCloser(bytes.NewReader(f.audio))}, nil
}

func (f *fakePolly) requests() []*polly.SynthesizeSpeechInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*polly.SynthesizeSpeechInput, len(f.inputs))
	copy(out, f.inputs)
	return out
}

// TestPollySynthesize drives the client directly: the request carries the fixed
// voice/engine/format and the MP3 response lands at outPath.
func TestPollySynthesize(t *testing.T) {
	fake := &fakePolly{audio: []byte("ID3fake-mp3-bytes")}
	c := &pollyClient{api: fake, voice: "Brian", engine: "neural", sampleRate: "24000", logger: log.New(io.Discard, "", 0)}

	out := filepath.Join(t.TempDir(), "out.mp3")
	if err := c.Synthesize(context.Background(), "hello clear", "ignored-voice-code", out); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if string(data) != "ID3fake-mp3-bytes" {
		t.Errorf("output=%q want the MP3 bytes from Polly", string(data))
	}

	reqs := fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests=%d want 1", len(reqs))
	}
	in := reqs[0]
	if aws.ToString(in.Text) != "hello clear" {
		t.Errorf("Text=%q want %q", aws.ToString(in.Text), "hello clear")
	}
	if string(in.VoiceId) != "Brian" {
		t.Errorf("VoiceId=%q want Brian", in.VoiceId)
	}
	if string(in.Engine) != "neural" {
		t.Errorf("Engine=%q want neural", in.Engine)
	}
	if string(in.OutputFormat) != "mp3" {
		t.Errorf("OutputFormat=%q want mp3", in.OutputFormat)
	}
	if aws.ToString(in.SampleRate) != "24000" {
		t.Errorf("SampleRate=%q want 24000", aws.ToString(in.SampleRate))
	}
	if string(in.TextType) != "text" {
		t.Errorf("TextType=%q want text", in.TextType)
	}
}

// TestPollyThroughQueue exercises the real path: POST /say -> Queue worker -> Polly
// client -> player, and confirms the queue writes tts-<id>.mp3 (the Ext() seam).
func TestPollyThroughQueue(t *testing.T) {
	fake := &fakePolly{audio: []byte("ID3-polly-mp3")}
	logger := log.New(io.Discard, "", 0)
	c := &pollyClient{api: fake, voice: "Brian", engine: "neural", sampleRate: "24000", logger: logger}

	player := &recPlayer{ch: make(chan struct{}, 4)}
	q := NewQueue(c, player, t.TempDir(), 100, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx)

	ts := httptest.NewServer(NewServer(q, nil, nil, "", 500, logger).Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{"text": "this is much clearer"})
	resp, err := http.Post(ts.URL+"/say", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/say status=%d want 202", resp.StatusCode)
	}
	waitPlay(t, player.ch)

	recs := player.records()
	if len(recs) != 1 {
		t.Fatalf("plays=%d want 1", len(recs))
	}
	if recs[0].data != "ID3-polly-mp3" {
		t.Errorf("played data=%q want the Polly MP3 bytes", recs[0].data)
	}
	if !strings.HasPrefix(recs[0].base, "tts-") || !strings.HasSuffix(recs[0].base, ".mp3") {
		t.Errorf("played file=%q want tts-<id>.mp3", recs[0].base)
	}
}
