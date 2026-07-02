package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Engine manages a persistent Python "sidecar" process that loads the Kokoro
// model once and synthesizes speech on demand. Communication is line-delimited
// JSON over the child's stdin/stdout (see pysidecar/tts_sidecar.py); the child's
// stderr is streamed to the logger.
//
// A supervisor goroutine (Run) owns the process lifecycle and restarts the
// child if it exits. A single reader goroutine owns stdout and dispatches
// responses to callers via per-request channels, so Synthesize is safe to call
// concurrently even though the queue worker calls it serially.
type Engine struct {
	python string
	script string
	speed  float64
	env    []string
	logger *log.Logger

	mu      sync.Mutex
	stdin   io.WriteCloser
	seq     int64
	pending map[int64]chan result
	ready   bool
	readyCh chan struct{}
	closed  bool
}

type result struct {
	ok  bool
	err string
}

// NewEngine builds an Engine. lang/voice seed the child's environment so the
// sidecar can default them; python is the interpreter path and script the
// sidecar entrypoint.
func NewEngine(python, script, lang, voice string, speed float64, logger *log.Logger) *Engine {
	env := append(os.Environ(),
		"PYTORCH_ENABLE_MPS_FALLBACK=1", // allow MPS GPU on Apple Silicon
		"HF_HUB_DISABLE_PROGRESS_BARS=1",
		"TRANSFORMERS_VERBOSITY=error",
		"TOKENIZERS_PARALLELISM=false",
		"PYTHONUNBUFFERED=1",
		"TTS_LANG="+lang,
		"TTS_VOICE="+voice,
	)
	return &Engine{
		python:  python,
		script:  script,
		speed:   speed,
		env:     env,
		logger:  logger,
		pending: make(map[int64]chan result),
	}
}

// Run supervises the sidecar until ctx is canceled, restarting it with
// exponential backoff if it exits.
func (e *Engine) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := e.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			e.logger.Printf("sidecar: %v", err)
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second // it ran fine for a while; reset backoff
		}
		e.logger.Printf("sidecar restarting in %s", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

func (e *Engine) runOnce(ctx context.Context) error {
	cmd := exec.Command(e.python, e.script)
	cmd.Env = e.env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting sidecar (%s %s): %w", e.python, e.script, err)
	}
	e.logger.Printf("sidecar started (pid %d), loading model…", cmd.Process.Pid)

	readyCh := make(chan struct{})
	e.mu.Lock()
	e.stdin = stdin
	e.ready = false
	e.readyCh = readyCh
	e.mu.Unlock()

	go e.pipeStderr(stderr)

	readDone := make(chan struct{})
	go func() { e.readLoop(stdout, readyCh); close(readDone) }()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		<-waitErr
		e.tearDown("sidecar shutting down")
		<-readDone
		return ctx.Err()
	case err := <-waitErr:
		e.tearDown("sidecar process exited")
		<-readDone
		return fmt.Errorf("process exited: %v", err)
	}
}

// readLoop consumes protocol JSON from the child's stdout. Non-JSON noise is
// ignored so stray library output can't wedge the protocol.
func (e *Engine) readLoop(r io.Reader, readyCh chan struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg struct {
			Type  string `json:"type"`
			ID    int64  `json:"id"`
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "ready":
			e.mu.Lock()
			e.ready = true
			e.mu.Unlock()
			select {
			case <-readyCh:
			default:
				close(readyCh)
			}
			e.logger.Printf("sidecar ready")
		case "result":
			e.mu.Lock()
			ch := e.pending[msg.ID]
			delete(e.pending, msg.ID)
			e.mu.Unlock()
			if ch != nil {
				ch <- result{ok: msg.OK, err: msg.Error}
			}
		}
	}
}

// pipeStderr streams the sidecar's stderr into the logger. All library noise
// (torch/transformers warnings, HuggingFace downloads) lands here.
func (e *Engine) pipeStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		e.logger.Printf("[sidecar] %s", sc.Text())
	}
}

// tearDown marks the engine not-ready and fails any in-flight requests.
func (e *Engine) tearDown(reason string) {
	e.mu.Lock()
	e.ready = false
	e.stdin = nil
	for id, ch := range e.pending {
		ch <- result{ok: false, err: reason}
		delete(e.pending, id)
	}
	e.mu.Unlock()
}

// Synthesize renders text to a WAV at outPath, blocking until the sidecar
// reports completion or ctx is canceled. It waits for the model to finish
// loading if necessary.
func (e *Engine) Synthesize(ctx context.Context, text, voice, outPath string) error {
	e.mu.Lock()
	ready, readyCh := e.ready, e.readyCh
	e.mu.Unlock()
	if !ready {
		if readyCh == nil {
			return errors.New("engine not started")
		}
		select {
		case <-readyCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	e.mu.Lock()
	if e.closed || e.stdin == nil || !e.ready {
		e.mu.Unlock()
		return errors.New("engine not ready")
	}
	e.seq++
	id := e.seq
	ch := make(chan result, 1)
	e.pending[id] = ch
	stdin := e.stdin
	e.mu.Unlock()

	req, _ := json.Marshal(map[string]any{
		"id":    id,
		"text":  text,
		"voice": voice,
		"speed": e.speed,
		"out":   outPath,
	})
	if _, err := stdin.Write(append(req, '\n')); err != nil {
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return fmt.Errorf("writing to sidecar: %w", err)
	}

	select {
	case res := <-ch:
		if !res.ok {
			return fmt.Errorf("synthesis failed: %s", res.err)
		}
		return nil
	case <-ctx.Done():
		e.mu.Lock()
		delete(e.pending, id)
		e.mu.Unlock()
		return ctx.Err()
	}
}

// Ready reports whether the model is loaded and the engine can synthesize.
func (e *Engine) Ready() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ready
}
