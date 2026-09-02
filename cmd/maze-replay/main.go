// Command maze-replay re-runs a stored GET OUT!!! round through the overlay.
//
// ADR-0004 built the archive and named this as the missing half: the data is what
// is irrecoverable, and a viewer can be built from it at any time. This is that
// viewer. It reads a round out of maze_rounds, restores the board and rules it was
// played under, feeds the recorded moves back through the engine a turn at a time,
// and pushes the result at the same endpoint the bot pushes a live game to.
//
// It replays rather than summarises because it can: internal/maze contains no
// randomness at all, so the same board and the same submissions in the same order
// produce the same game. That is the property the archive was designed around.
//
//	maze-replay -list                     # what is in the archive
//	maze-replay -last                     # watch the most recent round
//	maze-replay -round <id> -speed 3      # a particular one, three times as fast
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"tts/internal/maze"
	"tts/internal/mazearchive"
	"tts/internal/mazeview"
	"tts/store"
	"tts/store/postgres"
	"tts/store/sqlite"
)

// replayStore is what the tool needs from a backend: the three archive reads, and
// the in-flight round lookup it refuses to run over. Narrow on purpose — this is a
// reader, and a type that cannot write cannot be the thing that damaged an archive
// whose whole value is being permanent.
type replayStore interface {
	MazeRoundLog(n int) ([]store.MazeRound, error)
	MazeRoundByID(id string) (store.MazeRound, bool, error)
	MazeRoundEvents(id string) ([]store.MazeEvent, error)
	LoadRound(game string) (store.Round, bool, error)
	Close() error
}

func main() {
	dsn := flag.String("db", envOr("TTS_DATABASE_URL", "bot.db"), "sqlite path or postgres:// DSN")
	url := flag.String("url", envOr("TTS_URL", "http://127.0.0.1:8080"), "TTS server base URL")
	token := flag.String("token", os.Getenv("TTS_TOKEN"), "TTS server token")
	list := flag.Bool("list", false, "list recent rounds and exit")
	last := flag.Bool("last", false, "replay the most recent round")
	round := flag.String("round", "", "replay this round id")
	speed := flag.Float64("speed", 1, "playback multiplier; 2 is twice as fast as it was played")
	display := flag.String("display", "", `"panel" or "full"; defaults to how the round was played`)
	from := flag.Int("from", 0, "skip ahead to this turn")
	flag.Parse()

	db, err := open(*dsn)
	if err != nil {
		die("open %s: %v", *dsn, err)
	}
	defer db.Close()

	if *list {
		if err := listRounds(db); err != nil {
			die("%v", err)
		}
		return
	}
	if *speed <= 0 {
		die("-speed must be positive")
	}

	rec, err := pick(db, *round, *last)
	if err != nil {
		die("%v", err)
	}

	// One maze slot in the overlay's state cache means a replay and a live round
	// fight over the board and both look broken. This is not hypothetical: two
	// rounds were driven into that slot during a playtest and the board flickered
	// between two games.
	if live, ok, err := db.LoadRound(store.GameMaze); err != nil {
		die("checking for a live round: %v", err)
	} else if ok {
		die("a round is live on the stage right now (room %s) — a replay would fight it for the board", live.RoomID)
	}

	if err := play(db, rec, *url, *token, *speed, *display, *from); err != nil {
		die("%v", err)
	}
}

func play(db replayStore, rec store.MazeRound, url, token string, speed float64, display string, from int) error {
	var rep mazearchive.Replay
	if err := json.Unmarshal(rec.Input, &rep); err != nil {
		return fmt.Errorf("round %s: input is not a replay document: %w", rec.ID, err)
	}
	rd, err := mazearchive.Reconstruct(rep)
	if err != nil {
		return fmt.Errorf("round %s: %w", rec.ID, err)
	}
	if rep.Opening == nil {
		fmt.Fprintf(os.Stderr, "note: round %s predates the opening state being recorded; rebuilt from its board and seats\n", rec.ID)
	}

	if display == "" {
		display = rep.Display
	}
	if display == "" {
		display = "full"
	}
	tick := time.Duration(rec.TickMS) * time.Millisecond
	beat := time.Duration(0)
	if rep.ResolveMS != nil {
		beat = time.Duration(*rep.ResolveMS) * time.Millisecond
	}
	period := tick + beat

	run := mazearchive.NewRunner(rep, period)
	push := pusher(url, token)
	opts := func(feed []string) mazeview.Options {
		return mazeview.Options{
			RoundID: rec.ID, Display: display, TickMS: rec.TickMS,
			CycleMsLeft: period.Milliseconds(), Feed: feed,
		}
	}

	fmt.Printf("replaying %s — %d turns, %d players, %s, %s a turn at %.1fx\n",
		rec.ID, rec.Cycles, rec.Players, rec.Reason, period, speed)

	if err := push(mazeview.Build(rd, opts(nil))); err != nil {
		return err
	}

	// The feed is built from the events the replay itself produces, through the
	// same formatter the bot uses — so the play-by-play is the real one rather than
	// a second rendering that could word it differently. The stored events are then
	// a check on all of it: if they disagree, this replay is not the round.
	var feed []string
	var replayed []string
	for guard := 0; rd.Phase != maze.PhaseDone; guard++ {
		if guard > rec.Cycles+rd.Cfg.JoinCycles+10 {
			return fmt.Errorf("replay stuck on turn %d in %v", rd.Cycle, rd.Phase)
		}
		evs, err := run.Step(rd)
		if err != nil {
			return err
		}
		for _, e := range evs {
			replayed = append(replayed, e.Kind.String())
			name := ""
			if p := seatName(rd, e.Seat); p != "" {
				name = p
			}
			if line, ok := mazeview.FeedLine(e, name); ok {
				feed = append(feed, line)
			}
		}
		if len(feed) > mazeview.FeedLines {
			feed = feed[len(feed)-mazeview.FeedLines:]
		}
		if rd.Cycle < from {
			continue
		}
		if err := push(mazeview.Build(rd, opts(feed))); err != nil {
			return err
		}
		time.Sleep(time.Duration(float64(period) / speed))
	}

	if err := checkAgainstLog(db, rec, rd, replayed); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	fmt.Printf("done — %s\n", rec.Reason)
	return push(map[string]any{"hidden": true})
}

// pusher posts a payload to the overlay, wrapped exactly as the bot wraps one.
func pusher(base, token string) func(data any) error {
	u := strings.TrimRight(base, "/") + "/overlay/state"
	if token != "" {
		u += "?token=" + token
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(data any) error {
		body, err := json.Marshal(map[string]any{"kind": "maze", "data": data})
		if err != nil {
			return err
		}
		resp, err := client.Post(u, "application/json", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("push: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("push: /overlay/state -> %s", resp.Status)
		}
		return nil
	}
}

func pick(db replayStore, id string, last bool) (store.MazeRound, error) {
	switch {
	case id != "":
		rec, ok, err := db.MazeRoundByID(id)
		if err != nil {
			return rec, err
		}
		if !ok {
			return rec, fmt.Errorf("no round %s in the archive — try -list", id)
		}
		return rec, nil
	case last:
		rounds, err := db.MazeRoundLog(1)
		if err != nil {
			return store.MazeRound{}, err
		}
		if len(rounds) == 0 {
			return store.MazeRound{}, fmt.Errorf("the archive is empty")
		}
		return rounds[0], nil
	}
	return store.MazeRound{}, fmt.Errorf("say which round: -last, -round <id>, or -list to see them")
}

func listRounds(db replayStore) error {
	rounds, err := db.MazeRoundLog(20)
	if err != nil {
		return err
	}
	if len(rounds) == 0 {
		fmt.Println("the archive is empty")
		return nil
	}
	for _, r := range rounds {
		winner := r.WinnerDisplay
		if winner == "" {
			winner = "nobody"
		}
		fmt.Printf("%s  %s  %3d turns  %d/%d out  won by %-16s  %s\n",
			r.ID, time.Unix(r.StartedAt, 0).Format("2006-01-02 15:04"),
			r.Cycles, r.Finishers, r.Players, winner, r.Reason)
	}
	return nil
}

// seatName is the display name for a seat, or "" for a round-level event.
func seatName(rd *maze.Round, seat int) string {
	for _, p := range rd.Players {
		if p.Seat == seat {
			return p.Display
		}
	}
	return ""
}

// checkAgainstLog compares what the replay emitted with what the round recorded.
//
// Every replay is therefore its own proof. The engine is deterministic, so these
// must match; if they do not, something about the stored round no longer
// reproduces it and the thing on screen is not the game that was played — which is
// worth saying out loud rather than letting somebody draw conclusions from it.
func checkAgainstLog(db replayStore, rec store.MazeRound, rd *maze.Round, replayed []string) error {
	evs, err := db.MazeRoundEvents(rec.ID)
	if err != nil {
		return fmt.Errorf("could not read the stored events to check against: %w", err)
	}
	var stored []string
	for _, e := range evs {
		stored = append(stored, e.Kind)
	}
	if rd.Cycle != rec.Cycles {
		return fmt.Errorf("replay ended on turn %d, the round ended on %d", rd.Cycle, rec.Cycles)
	}
	if len(stored) != len(replayed) {
		return fmt.Errorf("replay emitted %d events, the round recorded %d", len(replayed), len(stored))
	}
	for i := range stored {
		if stored[i] != replayed[i] {
			return fmt.Errorf("event %d was %q in the round and %q in the replay", i, stored[i], replayed[i])
		}
	}
	return nil
}

func open(dsn string) (replayStore, error) {
	backend, target := store.Classify(dsn)
	if backend == store.PostgresBackend {
		return postgres.Open(target)
	}
	return sqlite.Open(target)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "maze-replay: "+format+"\n", args...)
	os.Exit(1)
}
