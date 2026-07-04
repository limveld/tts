package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// ErrQueueFull is returned by Enqueue when the pending queue is at capacity.
var ErrQueueFull = errors.New("queue is full")

// QueueItem is one pending or in-flight job. Kind "" or "tts" is synthesized
// from Text/Voice; kind "sfx" plays the pre-recorded file at Src (Text holds the
// sound's command name, for logging/status).
type QueueItem struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind,omitempty"`
	Text  string `json:"text"`
	Voice string `json:"voice,omitempty"`
	Src   string `json:"src,omitempty"`
}

// Status is a snapshot of the queue for the /status endpoint.
type Status struct {
	Paused      bool        `json:"paused"`
	EngineReady bool        `json:"engineReady"`
	QueueLength int         `json:"queueLength"`
	Current     *QueueItem  `json:"current"`
	Pending     []QueueItem `json:"pending"`
}

// Queue serializes TTS jobs through a single worker: each job is synthesized by
// the engine, then played by the player, one at a time. It supports pause
// (stop starting new items; the current one finishes), clear (drop pending),
// and skip (cancel the current item's synth+playback).
type Queue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	items  []QueueItem
	paused bool
	nextID int64
	maxLen int

	current       *QueueItem
	cancelCurrent context.CancelFunc

	engine *Engine
	player Player
	tmpDir string
	logger *log.Logger
}

// NewQueue constructs a Queue. maxLen caps the number of pending items.
func NewQueue(engine *Engine, player Player, tmpDir string, maxLen int, logger *log.Logger) *Queue {
	q := &Queue{
		engine: engine,
		player: player,
		tmpDir: tmpDir,
		maxLen: maxLen,
		logger: logger,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Enqueue appends a job and wakes the worker. It returns the job id and its
// 1-based position in the pending queue, or ErrQueueFull.
func (q *Queue) Enqueue(text, voice string) (id int64, position int, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= q.maxLen {
		return 0, 0, ErrQueueFull
	}
	q.nextID++
	item := QueueItem{ID: q.nextID, Text: text, Voice: voice}
	q.items = append(q.items, item)
	q.cond.Signal()
	return item.ID, len(q.items), nil
}

// EnqueueSFX appends a sound-effect job (a pre-recorded clip at srcPath, played
// through the same worker as TTS) and wakes the worker. name is the sound's
// command, used only for logging/status.
func (q *Queue) EnqueueSFX(name, srcPath string) (id int64, position int, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) >= q.maxLen {
		return 0, 0, ErrQueueFull
	}
	q.nextID++
	item := QueueItem{ID: q.nextID, Kind: "sfx", Text: name, Src: srcPath}
	q.items = append(q.items, item)
	q.cond.Signal()
	return item.ID, len(q.items), nil
}

// Pause stops the worker from starting new items. A currently-playing item
// finishes normally.
func (q *Queue) Pause() {
	q.mu.Lock()
	q.paused = true
	q.mu.Unlock()
}

// Resume lets the worker start items again.
func (q *Queue) Resume() {
	q.mu.Lock()
	q.paused = false
	q.cond.Broadcast()
	q.mu.Unlock()
}

// Clear drops all pending items and returns how many were removed. It does not
// affect the currently-playing item.
func (q *Queue) Clear() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	q.items = nil
	return n
}

// Skip cancels the current item's synthesis/playback, advancing to the next.
// It returns true if there was something to skip.
func (q *Queue) Skip() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cancelCurrent != nil {
		q.cancelCurrent()
		return true
	}
	return false
}

// Status returns a snapshot of the queue.
func (q *Queue) Status() Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	pending := make([]QueueItem, len(q.items))
	copy(pending, q.items)
	var current *QueueItem
	if q.current != nil {
		current = new(*q.current)
	}
	return Status{
		Paused:      q.paused,
		EngineReady: q.engine != nil && q.engine.Ready(),
		QueueLength: len(q.items),
		Current:     current,
		Pending:     pending,
	}
}

// Run is the worker loop. It blocks until ctx is canceled.
func (q *Queue) Run(ctx context.Context) {
	// Wake the worker on shutdown so it can exit cond.Wait.
	go func() {
		<-ctx.Done()
		q.mu.Lock()
		q.cond.Broadcast()
		q.mu.Unlock()
	}()

	for {
		q.mu.Lock()
		for (len(q.items) == 0 || q.paused) && ctx.Err() == nil {
			q.cond.Wait()
		}
		if ctx.Err() != nil {
			q.mu.Unlock()
			return
		}
		item := q.items[0]
		q.items = q.items[1:]
		jobCtx, cancel := context.WithCancel(ctx)
		q.current = &item
		q.cancelCurrent = cancel
		q.mu.Unlock()

		q.process(jobCtx, item)

		cancel()
		q.mu.Lock()
		q.current = nil
		q.cancelCurrent = nil
		q.mu.Unlock()
	}
}

// process runs a single item: synthesize (TTS) or fetch the local clip (SFX),
// then play, cleaning up the temp file.
func (q *Queue) process(ctx context.Context, item QueueItem) {
	if item.Kind == "sfx" {
		q.processSFX(ctx, item)
		return
	}

	wav := filepath.Join(q.tmpDir, fmt.Sprintf("tts-%d.wav", item.ID))
	defer os.Remove(wav)

	if err := q.engine.Synthesize(ctx, item.Text, item.Voice, wav); err != nil {
		if ctx.Err() != nil {
			q.logger.Printf("job %d skipped during synthesis", item.ID)
		} else {
			q.logger.Printf("job %d synthesis error: %v", item.ID, err)
		}
		return
	}

	q.logger.Printf("job %d playing", item.ID)
	q.play(ctx, item.ID, wav)
}

// processSFX plays a pre-recorded clip. The clip is copied into the temp dir as
// tts-<id><ext> so the same id-keyed player/overlay path serves it; the copy is
// removed afterward while the sfx-library original is left untouched.
func (q *Queue) processSFX(ctx context.Context, item QueueItem) {
	ext := filepath.Ext(item.Src)
	if ext == "" {
		ext = ".mp3"
	}
	clip := filepath.Join(q.tmpDir, fmt.Sprintf("tts-%d%s", item.ID, ext))
	if err := copyFile(item.Src, clip); err != nil {
		q.logger.Printf("job %d sfx %q copy error: %v", item.ID, item.Text, err)
		return
	}
	defer os.Remove(clip)

	q.logger.Printf("job %d playing sfx %q", item.ID, item.Text)
	q.play(ctx, item.ID, clip)
}

// play plays clip and logs the outcome (a skip cancels ctx).
func (q *Queue) play(ctx context.Context, id int64, clip string) {
	if err := q.player.Play(ctx, id, clip); err != nil {
		if ctx.Err() != nil {
			q.logger.Printf("job %d playback skipped", id)
		} else {
			q.logger.Printf("job %d playback error: %v", id, err)
		}
		return
	}
	q.logger.Printf("job %d done", id)
}

// copyFile copies src to dst (used to stage an sfx clip into the temp dir).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
