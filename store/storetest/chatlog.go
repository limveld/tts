package storetest

import (
	"testing"

	"tts/store"
)

// The chat log's contract. What matters across backends is that a batch round-
// trips intact, that the two tombstone shapes hit exactly the rows they name,
// and that a tombstone never touches the text — the moderation lookup exists to
// read the line that got someone banned.

// msg builds a chat line with the fields these cases care about set and the rest
// left at their zero values, so each test reads as the thing it is checking.
func msg(ts int64, msgID, userID, login, text string) store.ChatMessage {
	return store.ChatMessage{
		TS: ts, RoomID: "room1", MsgID: msgID,
		UserID: userID, Login: login, Display: login, Text: text,
	}
}

func testChatLogRoundTrip(t *testing.T, s Store) {
	in := []store.ChatMessage{
		msg(100, "m1", "u1", "bob", "hello"),
		msg(200, "m2", "u1", "bob", "again"),
		msg(300, "m3", "u2", "amy", "hi"),
	}
	// The third carries every optional field, so a batch insert that quietly
	// dropped one would fail here rather than in production six weeks later.
	in[2].Emotes = "25:0-4"
	in[2].IsMod, in[2].IsSub, in[2].IsVIP, in[2].IsBroadcaster = true, true, true, true

	if err := s.LogMessages(in); err != nil {
		t.Fatal(err)
	}

	got, err := s.UserMessages("u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("UserMessages(u1)=%d rows want 2", len(got))
	}
	// Newest first.
	if got[0].MsgID != "m2" || got[1].MsgID != "m1" {
		t.Errorf("order=%q,%q want m2,m1 (newest first)", got[0].MsgID, got[1].MsgID)
	}
	if got[0].Text != "again" || got[0].Login != "bob" || got[0].RoomID != "room1" {
		t.Errorf("row=%+v did not round-trip", got[0])
	}
	if got[0].ID == 0 {
		t.Error("ID=0: the row id did not come back")
	}
	if got[0].DeletedAt != 0 || got[0].DeletedBy != "" {
		t.Errorf("fresh row=%+v want live (deleted_at 0, deleted_by empty)", got[0])
	}

	amy, err := s.UserMessages("u2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(amy) != 1 {
		t.Fatalf("UserMessages(u2)=%d rows want 1", len(amy))
	}
	if amy[0].Emotes != "25:0-4" {
		t.Errorf("emotes=%q want 25:0-4", amy[0].Emotes)
	}
	if !amy[0].IsMod || !amy[0].IsSub || !amy[0].IsVIP || !amy[0].IsBroadcaster {
		t.Errorf("role bits=%+v want all true", amy[0])
	}

	// limit is a limit, not a suggestion.
	if one, _ := s.UserMessages("u1", 1); len(one) != 1 {
		t.Errorf("UserMessages(u1, 1) returned %d rows", len(one))
	}
}

func testChatLogEmptyBatch(t *testing.T, s Store) {
	// The writer flushes on a timer, so it will hand over an empty slice whenever
	// a tick lands in a quiet moment. Building "INSERT ... VALUES" with no rows is
	// a syntax error, so this is the one that would break at 3am on a dead stream.
	if err := s.LogMessages(nil); err != nil {
		t.Errorf("LogMessages(nil): %v", err)
	}
	if err := s.LogMessages([]store.ChatMessage{}); err != nil {
		t.Errorf("LogMessages(empty): %v", err)
	}
}

func testChatMarkDeleted(t *testing.T, s Store) {
	if err := s.LogMessages([]store.ChatMessage{
		msg(100, "m1", "u1", "bob", "oops"),
		msg(200, "m2", "u1", "bob", "fine"),
	}); err != nil {
		t.Fatal(err)
	}

	found, err := s.MarkDeleted("room1", "m1", 500)
	if err != nil || !found {
		t.Fatalf("MarkDeleted(m1): found=%v err=%v", found, err)
	}

	got, err := s.UserMessages("u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("UserMessages=%d rows want 2: a tombstone must not remove the row", len(got))
	}
	var deleted, live store.ChatMessage
	for _, m := range got {
		if m.MsgID == "m1" {
			deleted = m
		} else {
			live = m
		}
	}
	if deleted.DeletedAt != 500 || deleted.DeletedBy != "clearmsg" {
		t.Errorf("m1=%+v want deleted_at 500 by clearmsg", deleted)
	}
	if deleted.Text != "oops" {
		t.Errorf("m1 text=%q want it kept: the moderation question is what they said", deleted.Text)
	}
	if live.DeletedAt != 0 {
		t.Errorf("m2=%+v want untouched", live)
	}

	// A second tombstone on the same message finds nothing live to mark. Twitch
	// re-delivers, so this is a normal path.
	if found, err := s.MarkDeleted("room1", "m1", 600); err != nil || found {
		t.Errorf("re-delete: found=%v err=%v want false", found, err)
	}
	if got, _ := s.UserMessages("u1", 10); got[0].DeletedAt == 600 || got[1].DeletedAt == 600 {
		t.Error("re-delete overwrote the original tombstone; the first one wins")
	}

	// An unknown id is not an error: the message may predate logging, or have been
	// dropped when the writer's buffer was full.
	if found, err := s.MarkDeleted("room1", "nope", 700); err != nil || found {
		t.Errorf("unknown id: found=%v err=%v want false,nil", found, err)
	}
	// Nor is a known id in the wrong room.
	if found, err := s.MarkDeleted("room2", "m2", 700); err != nil || found {
		t.Errorf("wrong room: found=%v err=%v want false,nil", found, err)
	}
}

func testChatMarkUserCleared(t *testing.T, s Store) {
	if err := s.LogMessages([]store.ChatMessage{
		msg(100, "old", "u1", "bob", "ancient"),
		msg(500, "mid", "u1", "bob", "recent"),
		msg(900, "new", "u1", "bob", "newest"),
		msg(500, "amy", "u2", "amy", "innocent"),
	}); err != nil {
		t.Fatal(err)
	}

	// A ban clears the visible buffer, not the channel's whole history, so the
	// since bound is the point of this method.
	n, err := s.MarkUserCleared("room1", "u1", 500, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("MarkUserCleared marked %d rows want 2 (mid and new, not old)", n)
	}

	got, err := s.UserMessages("u1", 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.ChatMessage{}
	for _, m := range got {
		byID[m.MsgID] = m
	}
	if byID["old"].DeletedAt != 0 {
		t.Errorf("old=%+v want untouched: it is before the since bound", byID["old"])
	}
	for _, id := range []string{"mid", "new"} {
		if byID[id].DeletedAt != 1000 || byID[id].DeletedBy != "clearchat" {
			t.Errorf("%s=%+v want deleted_at 1000 by clearchat", id, byID[id])
		}
		if byID[id].Text == "" {
			t.Errorf("%s lost its text to a tombstone", id)
		}
	}

	// Another user in the same room is not collateral.
	if amy, _ := s.UserMessages("u2", 10); len(amy) != 1 || amy[0].DeletedAt != 0 {
		t.Errorf("u2=%+v want untouched", amy)
	}

	// Re-running is a no-op: the rows are already tombstoned, and the first
	// tombstone wins.
	if n, err := s.MarkUserCleared("room1", "u1", 500, 2000); err != nil || n != 0 {
		t.Errorf("re-clear marked %d rows err=%v want 0", n, err)
	}
}

func testChatTombstonesDoNotCrossType(t *testing.T, s Store) {
	// A CLEARCHAT arriving after a CLEARMSG must not relabel the earlier
	// tombstone: deleted_by has to stay the reason that actually removed the line,
	// or the log cannot tell "a mod deleted this" from "this person was banned".
	if err := s.LogMessages([]store.ChatMessage{msg(100, "m1", "u1", "bob", "hi")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDeleted("room1", "m1", 200); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkUserCleared("room1", "u1", 0, 300); err != nil || n != 0 {
		t.Fatalf("clearchat over a clearmsg marked %d rows err=%v want 0", n, err)
	}
	got, _ := s.UserMessages("u1", 10)
	if len(got) != 1 || got[0].DeletedBy != "clearmsg" || got[0].DeletedAt != 200 {
		t.Errorf("row=%+v want the original clearmsg tombstone intact", got)
	}
}
