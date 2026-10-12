package concierge

import (
	"errors"
	"testing"
	"time"
)

// A CreateTicket naming an id another ticket holds is refused with ErrDuplicateID
// on both stores, whichever partition holds the id: the stored ticket keeps every
// field it had, and the caller's struct keeps every field it was given, so a
// caller that retries with a fresh id is not left holding values from the refused
// write. A create with no id still mints one and fills the struct.
func TestTicket_ACallerSuppliedIDAnotherTicketHoldsIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		held := &Ticket{ID: "tk-held-id", Partition: "acme", UserID: "u1", Subject: "first", Status: "open"}
		if err := s.CreateTicket(ctx, held); err != nil {
			t.Fatalf("CreateTicket held: %v", err)
		}
		for _, partition := range []string{"acme", "globex"} {
			again := &Ticket{ID: "tk-held-id", Partition: partition, UserID: "u2", Subject: "second", Status: "open"}
			given := *again
			err := s.CreateTicket(ctx, again)
			if !errors.Is(err, ErrDuplicateID) {
				t.Fatalf("partition %q: CreateTicket over a held id: err = %v, want ErrDuplicateID", partition, err)
			}
			if again.Number != given.Number || !again.CreatedAt.Equal(given.CreatedAt) || !again.UpdatedAt.Equal(given.UpdatedAt) ||
				again.ID != given.ID || again.Subject != given.Subject {
				t.Fatalf("partition %q: a refused create changed the caller's struct: got %+v, want %+v", partition, *again, given)
			}
		}
		got, err := s.GetTicket(ctx, "acme", "tk-held-id")
		if err != nil {
			t.Fatalf("GetTicket held: %v", err)
		}
		if got.Subject != "first" || got.UserID != "u1" || got.Partition != "acme" {
			t.Fatalf("a refused create changed the stored ticket: %+v", got)
		}
		if _, err := s.GetTicket(ctx, "globex", "tk-held-id"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a refused create stored a row under the other partition: err = %v", err)
		}

		minted := &Ticket{Partition: "acme", UserID: "u3", Subject: "third", Status: "open"}
		if err := s.CreateTicket(ctx, minted); err != nil {
			t.Fatalf("CreateTicket with no id: %v", err)
		}
		if minted.ID == "" || minted.Number == "" || minted.CreatedAt.IsZero() || minted.UpdatedAt.Before(time.Unix(0, 1)) {
			t.Fatalf("a create with no id must mint the id and fill the struct: %+v", *minted)
		}
	})
}

// A caller-supplied id shorter than the ticket number's eight characters is
// stored, and becomes the number whole, on both stores: CreateTicket derives the
// number from the id, and a derivation that sliced past the id's end would panic
// the caller's goroutine on an ordinary input.
func TestTicket_AShortCallerSuppliedIDIsItsOwnNumber(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		short := &Ticket{ID: "tk-1", Partition: "acme", UserID: "u1", Subject: "short", Status: "open"}
		if err := s.CreateTicket(ctx, short); err != nil {
			t.Fatalf("CreateTicket with a short id: %v", err)
		}
		if short.Number != "tk-1" {
			t.Fatalf("number = %q, want the whole id %q", short.Number, "tk-1")
		}
		got, err := s.GetTicket(ctx, "acme", "tk-1")
		if err != nil || got.Number != "tk-1" {
			t.Fatalf("GetTicket: %+v %v", got, err)
		}
	})
}
