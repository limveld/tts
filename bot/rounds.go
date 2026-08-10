package main

import (
	"encoding/json"

	"tts/store"
)

// All three games (gamble, wordle, connections) persist an in-flight round the
// same way: marshal a record, write it under the game's name, decode it back at
// startup, delete it when the round is over. This file is that shared shape, so
// each game keeps only the parts that are actually about the game.
//
// The store never looks inside the document. RoomID and EndsAt are passed
// separately only so a round is legible in psql without decoding a blob — each
// game still reads both out of its own record.

const (
	gambleGame      = "gamble"
	wordleGame      = "wordle"
	connectionsGame = "connections"
)

// RoundStore is the in-flight-round slice of the store, shared by all three
// games (an interface so tests can substitute a fake). *sqlite.Store and
// *postgres.Store satisfy it.
type RoundStore interface {
	SaveRound(game, roomID string, endsAt int64, state []byte) error
	LoadRound(game string) (store.Round, bool, error)
	ClearRound(game string) error
}

// saveRound marshals v as game's in-flight round. Persistence failures are
// logged, never returned: a live round must not die because the store hiccuped —
// the worst case is that a restart loses it, which is the pre-durability
// behavior, not a corruption.
func (r *Router) saveRound(game, roomID string, endsAt int64, v any) {
	if r.store == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		r.logger.Printf("%s persist marshal: %v", game, err)
		return
	}
	if err := r.store.SaveRound(game, roomID, endsAt, b); err != nil {
		r.logger.Printf("%s persist: %v", game, err)
	}
}

// loadRoundInto decodes game's stored round into v. ok is false when nothing is
// stored or the document is unreadable — a corrupt round is dropped rather than
// resurrected, since there is no safe way to settle a round we can't read.
func (r *Router) loadRoundInto(game string, v any) (ok bool) {
	if r.store == nil {
		return false
	}
	rec, found, err := r.store.LoadRound(game)
	if err != nil {
		r.logger.Printf("%s load: %v", game, err)
		return false
	}
	if !found {
		return false
	}
	if err := json.Unmarshal(rec.State, v); err != nil {
		r.logger.Printf("%s load unmarshal: %v", game, err)
		r.clearRound(game)
		return false
	}
	return true
}

// clearRound drops game's stored round.
func (r *Router) clearRound(game string) {
	if r.store == nil {
		return
	}
	if err := r.store.ClearRound(game); err != nil {
		r.logger.Printf("%s clear persist: %v", game, err)
	}
}
