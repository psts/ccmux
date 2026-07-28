// Peers bus persistence: an append-only event log (peer_events) plus one
// delivery cursor per peer (peer_cursors, highest cumulatively-acked seq).
// Delivery correctness lives here: subscribe replays seq > cursor, acks only
// advance the cursor forward, and pruning never outruns an addressee's cursor.
package store

import (
	"database/sql"
	"errors"

	"ccmux.dev/ccmuxd/internal/model"
)

const peerEventCols = `seq,kind,from_id,from_name,from_summary,from_cwd,to_id,to_name,grp,text,request_id,behavior,sent_at`

// AppendPeerEvent stores one event and returns (also setting) its seq.
func (s *SQLite) AppendPeerEvent(ev *model.PeerEvent) (int64, error) {
	res, err := s.db.Exec(`
INSERT INTO peer_events (kind,from_id,from_name,from_summary,from_cwd,to_id,to_name,grp,text,request_id,behavior,sent_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.Kind, ev.FromID, ev.FromName, ev.FromSummary, ev.FromCWD,
		ev.ToID, ev.ToName, ev.Group, ev.Text, ev.RequestID, ev.Behavior, ev.SentAt)
	if err != nil {
		return 0, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	ev.Seq = seq
	return seq, nil
}

// PeerEventsAfter returns a peer's events with seq > afterSeq, in seq order —
// the replay a (re)subscribe or poll delivers.
func (s *SQLite) PeerEventsAfter(toID string, afterSeq int64) ([]*model.PeerEvent, error) {
	rows, err := s.db.Query(
		`SELECT `+peerEventCols+` FROM peer_events WHERE to_id=? AND seq>? ORDER BY seq`,
		toID, afterSeq)
	if err != nil {
		return nil, err
	}
	return scanPeerEvents(rows)
}

// PeerCursor returns a peer's highest acked seq (0 when it has none yet).
func (s *SQLite) PeerCursor(peerID string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT acked_seq FROM peer_cursors WHERE peer_id=?`, peerID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// AdvancePeerCursor moves a peer's cursor forward to seq; a stale (lower) ack
// is a no-op, so lost or reordered acks can never rewind delivery.
func (s *SQLite) AdvancePeerCursor(peerID string, seq int64) error {
	_, err := s.db.Exec(`
INSERT INTO peer_cursors (peer_id, acked_seq) VALUES (?,?)
ON CONFLICT(peer_id) DO UPDATE SET acked_seq = MAX(acked_seq, excluded.acked_seq)`,
		peerID, seq)
	return err
}

// PeerMailbox is one peer's durable delivery state as the database knows it:
// the substrate the mailbox hangs off (a pane, or "" for a pane-less session)
// and when a session last claimed it. It is what makes the mailbox collectable
// — a peer id is a one-way hash of its pane, so without the recorded pane_id
// nothing can tell a live mailbox from the leftovers of a pane deleted weeks ago.
type PeerMailbox struct {
	PeerID    string
	PaneID    string
	UpdatedAt int64
}

// TouchPeerMailbox records (or refreshes) the substrate behind a peer's
// mailbox. It deliberately never writes acked_seq: registering is not delivery,
// and an existing cursor must neither rewind (replaying delivered mail) nor
// advance (swallowing undelivered mail).
func (s *SQLite) TouchPeerMailbox(peerID, paneID string, now int64) error {
	_, err := s.db.Exec(`
INSERT INTO peer_cursors (peer_id, acked_seq, pane_id, updated_at) VALUES (?,0,?,?)
ON CONFLICT(peer_id) DO UPDATE SET pane_id=excluded.pane_id, updated_at=excluded.updated_at`,
		peerID, paneID, now)
	return err
}

// PeerMailboxes lists every durable mailbox — the garbage collector's input.
func (s *SQLite) PeerMailboxes() ([]PeerMailbox, error) {
	rows, err := s.db.Query(`SELECT peer_id, pane_id, updated_at FROM peer_cursors`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerMailbox
	for rows.Next() {
		var m PeerMailbox
		if err := rows.Scan(&m.PeerID, &m.PaneID, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeletePeerState erases one peer's mailbox: its cursor and the events
// addressed to it. Events it SENT are left alone — those live in other peers'
// mailboxes, and group history stays renderable after a peer departs.
func (s *SQLite) DeletePeerState(peerID string) error {
	if _, err := s.db.Exec(`DELETE FROM peer_events WHERE to_id=?`, peerID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM peer_cursors WHERE peer_id=?`, peerID)
	return err
}

// RecentPeerSenders returns the distinct peers that sent toID a message since
// sinceMillis — the permission-relay broadcast set, computed instead of stored
// so it survives daemon and client restarts alike.
func (s *SQLite) RecentPeerSenders(toID string, sinceMillis int64) ([]string, error) {
	rows, err := s.db.Query(`
SELECT DISTINCT from_id FROM peer_events
WHERE to_id=? AND kind=? AND sent_at>? AND from_id!=''`,
		toID, model.PeerEventMessage, sinceMillis)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PeerGroupMessages returns the most recent message events whose sender was in
// group (snapshotted at send time), oldest first — the read-only viewer's
// history. limit<=0 means no cap.
func (s *SQLite) PeerGroupMessages(group string, sinceMillis int64, limit int) ([]*model.PeerEvent, error) {
	q := `SELECT ` + peerEventCols + ` FROM peer_events WHERE grp=? COLLATE NOCASE AND kind=? AND sent_at>? ORDER BY seq DESC`
	args := []any{group, model.PeerEventMessage, sinceMillis}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	evs, err := scanPeerEvents(rows)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs, nil
}

// PrunePeerEvents deletes events older than beforeMillis that their addressee
// has acked. An unacked event is never pruned — and, unlike the old broker's
// prefix scan, it doesn't block pruning anything else.
func (s *SQLite) PrunePeerEvents(beforeMillis int64) (int64, error) {
	res, err := s.db.Exec(`
DELETE FROM peer_events WHERE sent_at<? AND seq <= COALESCE(
  (SELECT acked_seq FROM peer_cursors WHERE peer_id = peer_events.to_id), 0)`,
		beforeMillis)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanPeerEvents(rows *sql.Rows) ([]*model.PeerEvent, error) {
	defer rows.Close()
	var out []*model.PeerEvent
	for rows.Next() {
		ev := &model.PeerEvent{}
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.FromID, &ev.FromName, &ev.FromSummary, &ev.FromCWD,
			&ev.ToID, &ev.ToName, &ev.Group, &ev.Text, &ev.RequestID, &ev.Behavior, &ev.SentAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
