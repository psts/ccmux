// Relay state that has to outlive the daemon: outstanding tool-approval requests
// and cross-group reply licences.
//
// Both were in-memory only, and both are long-lived by nature — a permission
// dialog can sit open for hours and a reply grant lasts two. A daemon restart
// therefore silently broke conversations in progress: a delegator's "yes <id>"
// stopped matching any request and arrived at the worker as ordinary chat, and a
// teammate reached into from another project was told it could not send messages
// across projects when it tried to answer. Neither failure looked like a restart.
package store

// PermRequest is one outstanding tool-approval relay.
type PermRequest struct {
	WorkerID  string
	Resolved  bool
	CreatedAt int64
}

// SavePermRequest records or updates an outstanding request. requestID is stored
// as given; the caller owns case normalisation (the service lowercases).
func (s *SQLite) SavePermRequest(requestID, workerID string, resolved bool, createdAt int64) error {
	_, err := s.db.Exec(`
INSERT INTO peer_perm_requests (request_id,worker_id,resolved,created_at) VALUES (?,?,?,?)
ON CONFLICT(request_id) DO UPDATE SET worker_id=excluded.worker_id,
  resolved=excluded.resolved, created_at=excluded.created_at`,
		requestID, workerID, boolToInt(resolved), createdAt)
	return err
}

// LoadPermRequests returns every stored request keyed by request id.
func (s *SQLite) LoadPermRequests() (map[string]PermRequest, error) {
	rows, err := s.db.Query(`SELECT request_id,worker_id,resolved,created_at FROM peer_perm_requests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PermRequest{}
	for rows.Next() {
		var id, worker string
		var resolved int
		var created int64
		if err := rows.Scan(&id, &worker, &resolved, &created); err != nil {
			return nil, err
		}
		out[id] = PermRequest{WorkerID: worker, Resolved: resolved != 0, CreatedAt: created}
	}
	return out, rows.Err()
}

// DeletePermRequest removes one request.
func (s *SQLite) DeletePermRequest(requestID string) error {
	_, err := s.db.Exec(`DELETE FROM peer_perm_requests WHERE request_id=?`, requestID)
	return err
}

// PrunePermRequests drops requests created before cutoff, matching the service's
// TTL sweep so the table cannot outgrow the map it mirrors.
func (s *SQLite) PrunePermRequests(beforeMillis int64) error {
	_, err := s.db.Exec(`DELETE FROM peer_perm_requests WHERE created_at<?`, beforeMillis)
	return err
}

// SaveReplyGrant records that replier may answer sender until expiresAt.
func (s *SQLite) SaveReplyGrant(replier, sender string, expiresAt int64) error {
	_, err := s.db.Exec(`
INSERT INTO peer_reply_grants (replier,sender,expires_at) VALUES (?,?,?)
ON CONFLICT(replier,sender) DO UPDATE SET expires_at=excluded.expires_at`,
		replier, sender, expiresAt)
	return err
}

// LoadReplyGrants returns every stored grant as replier → sender → expiry.
func (s *SQLite) LoadReplyGrants() ([]ReplyGrant, error) {
	rows, err := s.db.Query(`SELECT replier,sender,expires_at FROM peer_reply_grants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplyGrant
	for rows.Next() {
		var g ReplyGrant
		if err := rows.Scan(&g.Replier, &g.Sender, &g.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ReplyGrant is one stored cross-group reply licence.
type ReplyGrant struct {
	Replier   string
	Sender    string
	ExpiresAt int64
}

// DeleteReplyGrant removes one grant.
func (s *SQLite) DeleteReplyGrant(replier, sender string) error {
	_, err := s.db.Exec(`DELETE FROM peer_reply_grants WHERE replier=? AND sender=?`, replier, sender)
	return err
}

// PruneReplyGrants drops grants that have expired.
func (s *SQLite) PruneReplyGrants(nowMillis int64) error {
	_, err := s.db.Exec(`DELETE FROM peer_reply_grants WHERE expires_at<=?`, nowMillis)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
