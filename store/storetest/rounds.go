package storetest

import (
	"encoding/json"
	"reflect"
	"testing"
)

// sameJSON compares two documents semantically. It must not be a byte compare:
// Postgres stores rounds as JSONB, which normalizes key order and whitespace, so
// a round read back is equal in meaning but not in bytes. Every game unmarshals
// the document, so nothing downstream cares — but a test that compared bytes
// would fail on Postgres and pass on SQLite, which is the worst kind of red.
func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal got %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal want %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func testRoundSaveLoadClear(t *testing.T, s Store) {
	if _, ok, err := s.LoadRound("gamble"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v want false/nil", ok, err)
	}

	doc := []byte(`{"id":"r1","roomID":"room1","buyIn":100,"endsAt":1723312860000,` +
		`"entrants":[{"userID":"u1","login":"bob","display":"Bob"}],"winner":-1}`)
	if err := s.SaveRound("gamble", "room1", 1723312860000, doc); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.LoadRound("gamble")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.Game != "gamble" || got.RoomID != "room1" || got.EndsAt != 1723312860000 {
		t.Errorf("load: %+v want game=gamble room=room1 endsAt=1723312860000", got)
	}
	if !sameJSON(t, got.State, doc) {
		t.Errorf("state=%s want (semantically) %s", got.State, doc)
	}
	if got.UpdatedAt == 0 {
		t.Error("updated_at not stamped")
	}

	// Games are independent.
	if err := s.SaveRound("wordle", "room2", 0, []byte(`{"answer":"ABOUT"}`)); err != nil {
		t.Fatal(err)
	}
	if g, _, _ := s.LoadRound("gamble"); !sameJSON(t, g.State, doc) {
		t.Errorf("gamble clobbered by wordle: %s", g.State)
	}

	// Clear is a delete, so "no round" has exactly one spelling and callers only
	// have to check ok.
	if err := s.ClearRound("gamble"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.LoadRound("gamble"); ok {
		t.Error("gamble still present after clear")
	}
	if _, ok, _ := s.LoadRound("wordle"); !ok {
		t.Error("clearing gamble removed wordle too")
	}
	// Clearing an absent round is not an error — settle paths call it freely.
	if err := s.ClearRound("gamble"); err != nil {
		t.Errorf("clear absent: %v", err)
	}
}

func testRoundOverwrite(t *testing.T, s Store) {
	if err := s.SaveRound("connections", "roomA", 100, []byte(`{"mistakes":0}`)); err != nil {
		t.Fatal(err)
	}
	// Every mutation of a live round re-saves it, so overwrite is the hot path:
	// it must replace every column, not merge.
	if err := s.SaveRound("connections", "roomB", 200, []byte(`{"mistakes":3}`)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.LoadRound("connections")
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.RoomID != "roomB" || got.EndsAt != 200 {
		t.Errorf("columns=%s/%d want roomB/200", got.RoomID, got.EndsAt)
	}
	if !sameJSON(t, got.State, []byte(`{"mistakes":3}`)) {
		t.Errorf("state=%s want {\"mistakes\":3}", got.State)
	}

	// There is exactly one row per game, not a history.
	if err := s.SaveRound("connections", "roomC", 0, []byte(`{"mistakes":9}`)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LoadRound("connections"); got.EndsAt != 0 {
		t.Errorf("endsAt=%d want 0 — a zero deadline must overwrite, not be skipped", got.EndsAt)
	}
}
