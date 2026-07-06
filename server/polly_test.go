package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand"
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

// TestPollySynthesize drives the client directly: it uses the per-request voice,
// looks up that voice's tier (falling back to the default), and writes the MP3.
func TestPollySynthesize(t *testing.T) {
	fake := &fakePolly{audio: []byte("ID3fake-mp3-bytes")}
	c := &pollyClient{
		api:         fake,
		voiceTiers:  map[string]string{"Ruth": "generative"}, // Brian not listed → default tier
		defaultTier: "neural",
		sampleRate:  "24000",
		logger:      log.New(io.Discard, "", 0),
	}

	out := filepath.Join(t.TempDir(), "out.mp3")
	if err := c.Synthesize(context.Background(), "hello clear", "Brian", out); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if string(data) != "ID3fake-mp3-bytes" {
		t.Errorf("output=%q want the MP3 bytes from Polly", string(data))
	}

	in := fake.requests()[0]
	if aws.ToString(in.Text) != "hello clear" {
		t.Errorf("Text=%q want %q", aws.ToString(in.Text), "hello clear")
	}
	if string(in.VoiceId) != "Brian" {
		t.Errorf("VoiceId=%q want the per-request voice Brian", in.VoiceId)
	}
	if string(in.Engine) != "neural" {
		t.Errorf("Engine=%q want the default tier neural (Brian has no override)", in.Engine)
	}
	if string(in.OutputFormat) != "mp3" || aws.ToString(in.SampleRate) != "24000" || string(in.TextType) != "text" {
		t.Errorf("format/rate/type = %q/%q/%q want mp3/24000/text", in.OutputFormat, aws.ToString(in.SampleRate), in.TextType)
	}

	// A voice with a per-voice tier override uses it.
	if err := c.Synthesize(context.Background(), "gen", "Ruth", out); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if got := string(fake.requests()[1].Engine); got != "generative" {
		t.Errorf("Ruth Engine=%q want generative (per-voice override)", got)
	}
}

// TestPollyThroughQueue exercises the real path: POST /say {code:"k"} → VoiceMap
// resolves to Polly/Kevin → Queue routes to the polly synth → tts-<id>.mp3 played.
func TestPollyThroughQueue(t *testing.T) {
	fake := &fakePolly{audio: []byte("ID3-polly-mp3")}
	logger := log.New(io.Discard, "", 0)
	c := &pollyClient{api: fake, voiceTiers: map[string]string{"Kevin": "neural"}, defaultTier: "neural", sampleRate: "24000", logger: logger}

	vc := &VoicesConfig{Polly: EngineVoices{Engine: "neural", Codes: map[string]CodeSpec{"k": {Voice: "Kevin", Weight: 1}}}}
	vm := vc.Resolver(map[string]bool{"polly": true}, rand.New(rand.NewSource(1)))

	player := &recPlayer{ch: make(chan struct{}, 4)}
	q := NewQueue(map[string]Synthesizer{"polly": c}, "polly", player, t.TempDir(), 100, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx)

	ts := httptest.NewServer(NewServer(q, nil, nil, vm, "", 500, logger).Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{"text": "this is much clearer", "code": "k"})
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
	if len(recs) != 1 || recs[0].data != "ID3-polly-mp3" {
		t.Fatalf("plays=%v want one Polly MP3 clip", recs)
	}
	if !strings.HasPrefix(recs[0].base, "tts-") || !strings.HasSuffix(recs[0].base, ".mp3") {
		t.Errorf("played file=%q want tts-<id>.mp3", recs[0].base)
	}
	if reqs := fake.requests(); len(reqs) != 1 || string(reqs[0].VoiceId) != "Kevin" {
		t.Fatalf("polly reqs=%v want 1 with VoiceId Kevin", reqs)
	}
}
