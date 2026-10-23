package store

import "context"

// Conversation bodies live on the computer and the phone, never in Relay storage.
func (s *Store) CanReadConversation(ctx context.Context, ownerID, deviceID, threadID string) (bool, error) {
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN devices d ON d.id=t.device_id
	 WHERE d.owner_id=? AND d.revoked_at IS NULL AND t.device_id=? AND t.thread_id=?)`, ownerID, deviceID, threadID).Scan(&allowed)
	return allowed, err
}
