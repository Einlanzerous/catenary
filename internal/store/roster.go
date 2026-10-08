package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// RosterRow is one person the caller may start a conversation with (CANT-267).
// `initials` is derived at serve time, as UserRow's is.
type RosterRow struct {
	ID          uuid.UUID
	DisplayName string
	Handle      string
}

// Roster returns every active person other than the viewer, ordered by display
// name and then id. A bot is never listed (`kind = 'person'`) and neither is a
// deactivated account. Unpaged: the size is bounded by the number of accounts.
func (s *Store) Roster(ctx context.Context, viewer uuid.UUID) ([]RosterRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, display_name, handle
		FROM users
		WHERE kind = 'person' AND deactivated_at IS NULL AND id <> $1
		ORDER BY display_name, id`, viewer)
	if err != nil {
		return nil, fmt.Errorf("store: roster: %w", err)
	}
	defer rows.Close()
	out := []RosterRow{}
	for rows.Next() {
		var r RosterRow
		if err := rows.Scan(&r.ID, &r.DisplayName, &r.Handle); err != nil {
			return nil, fmt.Errorf("store: roster: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: roster: %w", err)
	}
	return out, nil
}
