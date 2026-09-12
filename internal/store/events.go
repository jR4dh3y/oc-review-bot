package store

import "time"

// ReviewEvent is one dashboard progress entry in a review's activity feed.
type ReviewEvent struct {
	ID        int64
	ReviewID  int64
	Kind      string
	Message   string
	CreatedAt time.Time
}

// AppendReviewEvent persists one progress event. Callers treat a failure as
// lost observability, never as a review failure.
func (s *Store) AppendReviewEvent(reviewID int64, kind, message string) error {
	_, err := s.db.Exec(`INSERT INTO review_events (review_id, kind, message, created_at)
			VALUES (?, ?, ?, ?)`, reviewID, kind, message, now())
	return err
}

// ListReviewEvents returns a review's progress feed in insertion order.
func (s *Store) ListReviewEvents(reviewID int64) ([]ReviewEvent, error) {
	rows, err := s.db.Query(`SELECT id, review_id, kind, message, created_at
			FROM review_events WHERE review_id = ? ORDER BY id`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewEvent
	for rows.Next() {
		var ev ReviewEvent
		var createdAt string
		if err := rows.Scan(&ev.ID, &ev.ReviewID, &ev.Kind, &ev.Message, &createdAt); err != nil {
			return nil, err
		}
		ev.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, ev)
	}
	return out, rows.Err()
}
