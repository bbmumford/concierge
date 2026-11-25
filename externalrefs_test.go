package concierge

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// A dotted external ref key, the form channel adapters use ("discord.channel"),
// reads back as written on both stores, finds its ticket, and clears.
func TestExternalRefsWithDottedKeysRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", UserID: "u1", Status: "open", ExternalRefs: map[string]string{"slack.thread": "t-9"}}
		if err := s.CreateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
		if err := s.SetExternalRef(ctx, tk.ID, "discord.channel", "c-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetExternalRef(ctx, tk.ID, "discord.message", "m-1"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTicket(ctx, "acme", tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"slack.thread": "t-9", "discord.channel": "c-1", "discord.message": "m-1"}
		if len(got.ExternalRefs) != len(want) {
			t.Fatalf("external refs read back as %v, want %v", got.ExternalRefs, want)
		}
		for k, v := range want {
			if got.ExternalRefs[k] != v {
				t.Fatalf("external refs read back as %v, want %v", got.ExternalRefs, want)
			}
		}
		for k, v := range want {
			found, err := s.GetTicketByExternalRef(ctx, k, v)
			if err != nil || found.ID != tk.ID {
				t.Fatalf("GetTicketByExternalRef(%q, %q): %v, %v", k, v, found, err)
			}
		}
		if err := s.SetExternalRef(ctx, tk.ID, "discord.message", ""); err != nil {
			t.Fatal(err)
		}
		got, _ = s.GetTicket(ctx, "acme", tk.ID)
		if _, still := got.ExternalRefs["discord.message"]; still || got.ExternalRefs["discord.channel"] != "c-1" {
			t.Fatalf("after clearing discord.message: %v", got.ExternalRefs)
		}
	})
}

// A message's dotted external ref reads back as written on both stores.
func TestMessageExternalRefsWithDottedKeysRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", UserID: "u1", Status: "open"}
		if err := s.CreateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
		msg := &TicketMessage{TicketID: tk.ID, AuthorType: "user", Content: "hi", CreatedAt: time.Unix(1001, 0)}
		if err := s.AddMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMessageExternalRef(ctx, msg.ID, "discord.message", "m-2"); err != nil {
			t.Fatal(err)
		}
		msgs, err := s.ListMessages(ctx, "acme", tk.ID, false)
		if err != nil || len(msgs) != 1 || msgs[0].ExternalRefs["discord.message"] != "m-2" {
			t.Fatalf("message refs: %+v, %v", msgs, err)
		}
	})
}

// A key that is empty or holds a double quote cannot name a member and is
// refused by both stores; a lookup by one finds nothing.
func TestExternalRefsRefuseAKeyTheyCannotName(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", UserID: "u1", Status: "open"}
		if err := s.CreateTicket(ctx, tk); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"", `discord".x`} {
			if err := s.SetExternalRef(ctx, tk.ID, key, "v"); !errors.Is(err, ErrInvalidRefKey) {
				t.Fatalf("SetExternalRef(%q): got %v, want ErrInvalidRefKey", key, err)
			}
			if err := s.SetMessageExternalRef(ctx, "any", key, "v"); !errors.Is(err, ErrInvalidRefKey) {
				t.Fatalf("SetMessageExternalRef(%q): got %v, want ErrInvalidRefKey", key, err)
			}
			if _, err := s.GetTicketByExternalRef(ctx, key, "v"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("GetTicketByExternalRef(%q): got %v, want ErrNotFound", key, err)
			}
		}
	})
}

// A row the SQL store wrote before keys were quoted holds a dotted key as a
// nested object. It reads back under the dotted name, is found by it, and a
// new value for the key replaces the nested member.
func TestSQLStoreReadsANestedExternalRef(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("db.Close: %v", err)
		}
	})
	s := NewSQLStore(db)
	if err := s.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	tk := &Ticket{Partition: "acme", UserID: "u1", Status: "open"}
	if err := s.CreateTicket(ctx, tk); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE concierge_tickets SET external_refs = ? WHERE id = ?`,
		`{"discord":{"channel":"c-old","message":"m-old"}}`, tk.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTicket(ctx, "acme", tk.ID)
	if err != nil || got.ExternalRefs["discord.channel"] != "c-old" || got.ExternalRefs["discord.message"] != "m-old" {
		t.Fatalf("nested refs read back as %v, %v", got.ExternalRefs, err)
	}
	if found, err := s.GetTicketByExternalRef(ctx, "discord.channel", "c-old"); err != nil || found.ID != tk.ID {
		t.Fatalf("lookup of a nested ref: %v, %v", found, err)
	}
	if err := s.SetExternalRef(ctx, tk.ID, "discord.channel", "c-new"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTicket(ctx, "acme", tk.ID)
	if got.ExternalRefs["discord.channel"] != "c-new" || got.ExternalRefs["discord.message"] != "m-old" {
		t.Fatalf("after setting discord.channel: %v", got.ExternalRefs)
	}
	if _, err := s.GetTicketByExternalRef(ctx, "discord.channel", "c-old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the replaced nested value still finds the ticket: %v", err)
	}
}
