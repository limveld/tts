package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	"github.com/aws/aws-sdk-go-v2/service/polly/types"
)

// pollyAPI is the slice of the Polly SDK the client needs, so tests can inject a
// fake instead of calling AWS. *polly.Client satisfies it.
type pollyAPI interface {
	SynthesizeSpeech(context.Context, *polly.SynthesizeSpeechInput, ...func(*polly.Options)) (*polly.SynthesizeSpeechOutput, error)
}

// pollyClient is a Synthesizer backed by Amazon Polly (a cloud TTS API). It
// synthesizes with a fixed voice + engine chosen at startup and streams the MP3
// response to disk. Credentials/region come from the AWS default chain
// (environment, ~/.aws, IAM); nothing is stored here.
type pollyClient struct {
	api        pollyAPI
	voice      string // Polly VoiceId, e.g. "Brian"
	engine     string // "standard" | "neural" | "long-form" | "generative"
	sampleRate string // Hz; "24000" is native for neural/generative
	logger     *log.Logger
}

// newPollyClient loads AWS config (region + credentials via the default chain) and
// builds a Polly client. It fails fast with a clear message when the region or
// credentials are missing, rather than erroring on the first !tts.
func newPollyClient(ctx context.Context, region, voice, engine string, logger *log.Logger) (*pollyClient, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region: pass -polly-region, set AWS_REGION, or configure ~/.aws/config")
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return nil, fmt.Errorf("aws credentials: %w (run 'aws configure' or set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY)", err)
	}
	return &pollyClient{
		api:        polly.NewFromConfig(cfg),
		voice:      voice,
		engine:     engine,
		sampleRate: "24000",
		logger:     logger,
	}, nil
}

// Synthesize calls Polly's SynthesizeSpeech and streams the MP3 audio to outPath.
// The per-request voice argument is ignored (kokoro voice codes don't apply); the
// fixed startup voice/engine are used instead. ctx cancellation (a !skip) aborts
// the request.
func (c *pollyClient) Synthesize(ctx context.Context, text, _, outPath string) error {
	out, err := c.api.SynthesizeSpeech(ctx, &polly.SynthesizeSpeechInput{
		Text:         aws.String(text),
		VoiceId:      types.VoiceId(c.voice),
		Engine:       types.Engine(c.engine),
		OutputFormat: types.OutputFormatMp3,
		SampleRate:   aws.String(c.sampleRate),
		TextType:     types.TextTypeText,
	})
	if err != nil {
		return err
	}
	defer out.AudioStream.Close()

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, out.AudioStream); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Ready reports whether the engine can synthesize. Polly is a cloud API validated
// at startup, so there is nothing local to wait on.
func (c *pollyClient) Ready() bool { return true }

// Ext is the output file extension: Polly returns MP3 at 24 kHz.
func (c *pollyClient) Ext() string { return ".mp3" }
