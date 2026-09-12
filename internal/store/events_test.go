package store

import (
	"testing"
)

func TestReviewEventsAppendListCascadeAndUnknownReview(t *testing.T) {
	t.Run("append and list preserve order", func(t *testing.T) {
		s := testStore(t)
		review := reviewFixture(1, 1, 1, 1)
		if err := s.CreateReview(review); err != nil {
			t.Fatal(err)
		}
		seeded := []struct {
			kind    string
			message string
		}{
			{"request", "Review requested by alice"},
			{"prepare", "Claimed for processing"},
			{"checkout", "Repository snapshot ready"},
		}
		for _, event := range seeded {
			if err := s.AppendReviewEvent(review.ID, event.kind, event.message); err != nil {
				t.Fatalf("append %q: %v", event.kind, err)
			}
		}
		events, err := s.ListReviewEvents(review.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != len(seeded) {
			t.Fatalf("events = %+v, want %d entries", events, len(seeded))
		}
		for i, event := range events {
			if event.ID == 0 || event.ReviewID != review.ID {
				t.Fatalf("event %d = %+v, want positive ID and review %d", i, event, review.ID)
			}
			if event.Kind != seeded[i].kind || event.Message != seeded[i].message {
				t.Fatalf("event %d = %q/%q, want %q/%q", i, event.Kind, event.Message, seeded[i].kind, seeded[i].message)
			}
			if event.CreatedAt.IsZero() {
				t.Fatalf("event %d has zero created_at", i)
			}
		}
	})

	t.Run("deleting the review cascades", func(t *testing.T) {
		s := testStore(t)
		review := reviewFixture(1, 1, 1, 1)
		if err := s.CreateReview(review); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendReviewEvent(review.ID, "request", "Review requested by alice"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`DELETE FROM reviews WHERE id = ?`, review.ID); err != nil {
			t.Fatal(err)
		}
		events, err := s.ListReviewEvents(review.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 0 {
			t.Fatalf("events survived review delete: %+v", events)
		}
	})

	t.Run("unknown review returns empty not error", func(t *testing.T) {
		s := testStore(t)
		events, err := s.ListReviewEvents(987654)
		if err != nil {
			t.Fatalf("list events for unknown review: %v", err)
		}
		if len(events) != 0 {
			t.Fatalf("events = %+v, want none", events)
		}
	})
}
