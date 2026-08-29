package postgres

import (
	"database/sql"
	"strconv"
	"strings"

	"tts/store"
)

// The chat log: every PRIVMSG the bot sees, plus the tombstones Twitch's
// CLEARMSG and CLEARCHAT put on them. Append-only apart from those tombstones —
// nothing here ever rewrites a line's text.
//
// chat_message is partitioned by day on ts_at, so every INSERT has to write
// ts_at explicitly. There is no DEFAULT and no trigger that could fill it: 00005
// documents why Postgres permits neither on a partition key. See
// docs/adr/0003-chat-log.md.

// chatBatchRows caps how many rows go into one multi-VALUES insert. The writer
// in bot/chatlog.go flushes at a quarter of this, so the chunking below is a
// backstop rather than a code path with a use case — but LogMessages is exported
// and a caller with a bigger slice must not hit Postgres's 65535-parameters-per-
// statement limit. At 12 placeholders per row this leaves an order of magnitude
// of headroom.
const chatBatchRows = 500

// LogMessages appends a batch of chat lines. Freshly logged messages are always
// live, so deleted_at/deleted_by take their column defaults rather than being
// written.
func (s *Store) LogMessages(msgs []store.ChatMessage) error {
	for len(msgs) > 0 {
		n := min(len(msgs), chatBatchRows)
		if err := s.logBatch(msgs[:n]); err != nil {
			return err
		}
		msgs = msgs[n:]
	}
	return nil
}

func (s *Store) logBatch(msgs []store.ChatMessage) error {
	var b strings.Builder
	b.WriteString(`INSERT INTO chat_message (ts, ts_at, room_id, msg_id, user_id, login, display, text, emotes, is_mod, is_sub, is_vip, is_broadcaster) VALUES `)
	args := make([]any, 0, len(msgs)*12)
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte(',')
		}
		base := i * 12
		p := func(n int) string { return "$" + strconv.Itoa(base+n) }
		// ts appears twice off one placeholder: ts_at is derived from the same
		// value rather than from a second parameter, so the two cannot disagree
		// about what instant this row is. points.go writes the ledger the same way.
		b.WriteString("(" + p(1) + "::bigint,to_timestamp(" + p(1) + "::bigint)," +
			p(2) + "," + p(3) + "," + p(4) + "," + p(5) + "," + p(6) + "," +
			p(7) + "," + p(8) + "," + p(9) + "," + p(10) + "," + p(11) + "," + p(12) + ")")
		args = append(args, m.TS, m.RoomID, m.MsgID, m.UserID, m.Login, m.Display,
			m.Text, m.Emotes, m.IsMod, m.IsSub, m.IsVIP, m.IsBroadcaster)
	}
	_, err := s.db.Exec(b.String(), args...)
	return err
}

// MarkDeleted tombstones one message by Twitch's message id. found is false when
// no live row matches — a normal outcome, not an error: the message may predate
// logging, or have been dropped when the writer's buffer was full.
//
// The UPDATE has no time predicate, because a CLEARMSG says which message was
// deleted but not when it was sent. On a partitioned table that means an Append
// across every child using each one's local chat_message_msg index — roughly 90
// sub-millisecond probes at the default horizon, a handful of times per stream.
// Guessing a time window to prune with would trade correctness for nothing.
func (s *Store) MarkDeleted(roomID, msgID string, at int64) (found bool, err error) {
	res, err := s.db.Exec(
		`UPDATE chat_message SET deleted_at = $1, deleted_by = 'clearmsg'
		  WHERE room_id = $2 AND msg_id = $3 AND deleted_at = 0`,
		at, roomID, msgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkUserCleared tombstones a user's messages from since onward, which is what
// a CLEARCHAT (ban or timeout) does to the visible chat buffer. It is bounded on
// purpose: a ban clears what is on screen, not the channel's whole history, so
// tombstoning everything a person ever said would overstate what Twitch did.
//
// Unlike MarkDeleted this has a timestamp, so it filters on ts_at and prunes to
// the partitions that can actually hold matching rows. ts_at is the same instant
// as ts — migrate_test.go pins that for every row — so the two spellings select
// identically and only this one prunes.
//
// Rows already tombstoned are left alone: the first tombstone wins, and
// deleted_by stays the reason that actually removed the line.
func (s *Store) MarkUserCleared(roomID, userID string, since, at int64) (n int64, err error) {
	res, err := s.db.Exec(
		`UPDATE chat_message SET deleted_at = $1, deleted_by = 'clearchat'
		  WHERE room_id = $2 AND user_id = $3 AND ts_at >= to_timestamp($4::bigint) AND deleted_at = 0`,
		at, roomID, userID, since)
	if err != nil {
		return 0, err
	}
	n, _ = res.RowsAffected()
	return n, nil
}

// UserMessages returns a user's most recent limit lines, newest first, including
// tombstoned ones — a moderation lookup that hid deleted messages would hide the
// exact rows it was opened for.
//
// It orders on ts_at rather than ts so the scan can walk chat_message_user in
// index order; the SQLite twin orders on ts, which is the same instant and the
// only index it has. This is the read the conformance suite uses to prove the
// three write methods above did what they claim.
func (s *Store) UserMessages(userID string, limit int) ([]store.ChatMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, room_id, msg_id, user_id, login, display, text, emotes,
		        is_mod, is_sub, is_vip, is_broadcaster, deleted_at, deleted_by
		   FROM chat_message
		  WHERE user_id = $1
		  ORDER BY ts_at DESC, id DESC
		  LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChatMessages(rows)
}

// scanChatMessages drains rows in the column order every query in this file
// selects. The SQLite twin carries its own copy: the two backends are separate
// packages, and store is deliberately type-only so there is nowhere shared to
// put it. points.go and wordle.go already duplicate their scan loops the same
// way.
func scanChatMessages(rows *sql.Rows) ([]store.ChatMessage, error) {
	var out []store.ChatMessage
	for rows.Next() {
		var m store.ChatMessage
		if err := rows.Scan(&m.ID, &m.TS, &m.RoomID, &m.MsgID, &m.UserID, &m.Login,
			&m.Display, &m.Text, &m.Emotes, &m.IsMod, &m.IsSub, &m.IsVIP,
			&m.IsBroadcaster, &m.DeletedAt, &m.DeletedBy); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
