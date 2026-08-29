package sqlite

import (
	"database/sql"
	"strings"

	"tts/store"
)

// The chat log: every PRIVMSG the bot sees, plus the tombstones Twitch's
// CLEARMSG and CLEARCHAT put on them. Append-only apart from those tombstones.
//
// The Postgres twin partitions chat_message by day and therefore writes a ts_at
// column on every insert. There is no partitioning here and no ts_at, exactly as
// 00005 left the ledger, so these statements are the same queries with one fewer
// column. See docs/adr/0003-chat-log.md.

// chatBatchRows caps how many rows go into one multi-VALUES insert, matching the
// Postgres twin so a batch that succeeds on one backend cannot fail on the
// other. At 12 placeholders per row this is far under SQLite's variable limit.
const chatBatchRows = 500

// LogMessages appends a batch of chat lines. Freshly logged messages are always
// live, so deleted_at/deleted_by take their column defaults.
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
	b.WriteString(`INSERT INTO chat_message (ts, room_id, msg_id, user_id, login, display, text, emotes, is_mod, is_sub, is_vip, is_broadcaster) VALUES `)
	args := make([]any, 0, len(msgs)*12)
	for i, m := range msgs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?)")
		args = append(args, m.TS, m.RoomID, m.MsgID, m.UserID, m.Login, m.Display,
			m.Text, m.Emotes, m.IsMod, m.IsSub, m.IsVIP, m.IsBroadcaster)
	}
	_, err := s.db.Exec(b.String(), args...)
	return err
}

// MarkDeleted tombstones one message by Twitch's message id. found is false when
// no live row matches — a normal outcome, not an error: the message may predate
// logging, or have been dropped when the writer's buffer was full.
func (s *Store) MarkDeleted(roomID, msgID string, at int64) (found bool, err error) {
	res, err := s.db.Exec(
		`UPDATE chat_message SET deleted_at = ?, deleted_by = 'clearmsg'
		  WHERE room_id = ? AND msg_id = ? AND deleted_at = 0`,
		at, roomID, msgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkUserCleared tombstones a user's messages from since onward, which is what
// a CLEARCHAT (ban or timeout) does to the visible chat buffer. It is bounded on
// purpose: a ban clears what is on screen, not the channel's whole history.
//
// The Postgres twin filters on ts_at so it can prune partitions; there is
// nothing to prune here, so it filters on ts. Same instant, same rows.
//
// Rows already tombstoned are left alone: the first tombstone wins, and
// deleted_by stays the reason that actually removed the line.
func (s *Store) MarkUserCleared(roomID, userID string, since, at int64) (n int64, err error) {
	res, err := s.db.Exec(
		`UPDATE chat_message SET deleted_at = ?, deleted_by = 'clearchat'
		  WHERE room_id = ? AND user_id = ? AND ts >= ? AND deleted_at = 0`,
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
func (s *Store) UserMessages(userID string, limit int) ([]store.ChatMessage, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, room_id, msg_id, user_id, login, display, text, emotes,
		        is_mod, is_sub, is_vip, is_broadcaster, deleted_at, deleted_by
		   FROM chat_message
		  WHERE user_id = ?
		  ORDER BY ts DESC, id DESC
		  LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanChatMessages(rows)
}

// scanChatMessages drains rows in the column order every query in this file
// selects. The Postgres twin carries its own copy: the two backends are separate
// packages, and store is deliberately type-only so there is nowhere shared to
// put it.
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
