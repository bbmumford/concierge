package concierge

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var ctx = context.Background()

// eachStore runs a store-agnostic case against both the MemStore reference and the durable SQLStore,
// so the SQL store is held to the exact same semantics the MemStore defines.
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })
	t.Run("sql", func(t *testing.T) {
		db, err := sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1) // keep the :memory: DB on one connection
		t.Cleanup(func() { db.Close() })
		s := NewSQLStore(db)
		if err := s.InitSchema(ctx); err != nil {
			t.Fatalf("InitSchema: %v", err)
		}
		fn(t, s)
	})
}

func TestTicket_CreateGetPartitionScopeAndExternalRef(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", UserID: "u1", Email: "a@x", Subject: "help", Status: "open"}
		if err := s.CreateTicket(ctx, tk); err != nil || tk.ID == "" || tk.Number == "" {
			t.Fatalf("CreateTicket: %v id=%q", err, tk.ID)
		}
		// partition-scoped get: right partition resolves, wrong partition is ErrNotFound
		if got, err := s.GetTicket(ctx, "acme", tk.ID); err != nil || got.Subject != "help" {
			t.Fatalf("GetTicket acme: %+v %v", got, err)
		}
		if _, err := s.GetTicket(ctx, "globex", tk.ID); !errors.Is(err, ErrNotFound) {
			t.Fatal("a different partition must not see the ticket")
		}
		// An empty partition names no partition, so it reaches nothing — the same
		// answer a wrong partition gets. Asserting both against the one that works
		// keeps the three outcomes distinct: a matching partition returns the
		// ticket, and neither of the others can be mistaken for it.
		if _, err := s.GetTicket(ctx, "", tk.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an empty partition must reach nothing, got err=%v", err)
		}
		// external-ref link + lookup (channel-agnostic)
		if err := s.SetExternalRef(ctx, tk.ID, "discord.channel", "chan-99"); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTicketByExternalRef(ctx, "discord.channel", "chan-99")
		if err != nil || got.ID != tk.ID {
			t.Fatalf("GetTicketByExternalRef: %+v %v", got, err)
		}
		// clearing the ref removes the lookup
		if err := s.SetExternalRef(ctx, tk.ID, "discord.channel", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetTicketByExternalRef(ctx, "discord.channel", "chan-99"); !errors.Is(err, ErrNotFound) {
			t.Fatal("a cleared external ref must no longer resolve")
		}
	})
}

func TestTicket_StatusTimestampsAndBreach(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		now := time.Unix(1_000_000, 0).UTC()
		tk := &Ticket{Partition: "acme", Status: "open", CreatedAt: now.Add(-2 * time.Hour)}
		s.CreateTicket(ctx, tk)
		// resolving stamps ResolvedAt
		if err := s.UpdateTicketStatus(ctx, "acme", tk.ID, "resolved"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTicket(ctx, "acme", tk.ID)
		if got.ResolvedAt == nil {
			t.Fatal("resolving must stamp ResolvedAt")
		}
		// a wrong-partition update is denied
		if err := s.UpdateTicketStatus(ctx, "globex", tk.ID, "closed"); !errors.Is(err, ErrNotFound) {
			t.Fatal("a different partition must not update the ticket")
		}
		// breach: an open unresponded 2h-old ticket breaches a 1h first-response SLA
		open := &Ticket{Partition: "acme", Status: "open", CreatedAt: now.Add(-2 * time.Hour)}
		s.CreateTicket(ctx, open)
		breached, _ := s.ListBreachedTickets(ctx, time.Hour, 0, now)
		found := false
		for _, b := range breached {
			if b.ID == open.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("a 2h-old unresponded open ticket must breach a 1h first-response SLA; got %d", len(breached))
		}
		// a closed ticket never breaches, even past every threshold
		closed := &Ticket{Partition: "acme", Status: "open", CreatedAt: now.Add(-9 * time.Hour)}
		s.CreateTicket(ctx, closed)
		s.UpdateTicketStatus(ctx, "acme", closed.ID, "closed")
		past, _ := s.ListBreachedTickets(ctx, time.Hour, time.Hour, now)
		for _, b := range past {
			if b.ID == closed.ID {
				t.Fatal("a closed ticket must never appear as breached")
			}
		}
	})
}

func TestTicket_StatsAndPaging(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		for _, st := range []string{"open", "open", "waiting", "closed", "resolved"} {
			s.CreateTicket(ctx, &Ticket{Partition: "acme", UserID: "u1", Status: st, Category: "billing"})
		}
		s.CreateTicket(ctx, &Ticket{Partition: "other", UserID: "u2", Status: "open"})
		stats, _ := s.GetTicketStats(ctx, "acme", "u1")
		if stats.Total != 5 || stats.Open != 2 || stats.Waiting != 1 || stats.Closed != 2 {
			t.Fatalf("stats partition-scoped mismatch: %+v", stats)
		}
		// admin paging across partitions with a status filter + total count
		page, total, _ := s.ListAllTickets(ctx, "open", "", 2, 0)
		if total != 3 || len(page) != 2 {
			t.Fatalf("ListAllTickets(open, limit 2): page=%d total=%d", len(page), total)
		}
	})
}

func TestMessages_InternalFilterAndPartitionGuard(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", Status: "open"}
		s.CreateTicket(ctx, tk)
		s.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "user", Content: "hi", CreatedAt: time.Unix(1001, 0)})
		s.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "agent", Content: "note", IsInternal: true, CreatedAt: time.Unix(1002, 0)})
		// public view excludes internal
		if pub, _ := s.ListMessages(ctx, "acme", tk.ID, false); len(pub) != 1 || pub[0].Content != "hi" {
			t.Fatalf("public message list must exclude internal notes; got %+v", pub)
		}
		if all, _ := s.ListMessages(ctx, "acme", tk.ID, true); len(all) != 2 {
			t.Fatalf("internal-inclusive list must return both; got %d", len(all))
		}
		// wrong partition sees nothing
		if wrong, _ := s.ListMessages(ctx, "globex", tk.ID, true); len(wrong) != 0 {
			t.Fatal("a different partition must not see a ticket's messages")
		}
		// since filter (strictly after)
		if since, _ := s.ListMessagesSince(ctx, "acme", tk.ID, time.Unix(1001, 0), true); len(since) != 1 {
			t.Fatalf("ListMessagesSince must exclude messages at/before `since`; got %d", len(since))
		}
	})
}

func TestEvents_AppendOnlyAudit(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", Status: "open"}
		s.CreateTicket(ctx, tk)
		s.AddTicketEvent(ctx, &TicketEvent{TicketID: tk.ID, EventType: "status_change", OldValue: "open", NewValue: "resolved", CreatedAt: time.Unix(1001, 0)})
		ev, _ := s.ListTicketEvents(ctx, "acme", tk.ID)
		if len(ev) != 1 || ev[0].NewValue != "resolved" {
			t.Fatalf("event audit must record the transition; got %+v", ev)
		}
		if wrong, _ := s.ListTicketEvents(ctx, "globex", tk.ID); len(wrong) != 0 {
			t.Fatal("a different partition must not read the audit log")
		}
	})
}

func TestAttachments_PartitionGuard(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", Status: "open"}
		s.CreateTicket(ctx, tk)
		s.AddAttachment(ctx, &Attachment{TicketID: tk.ID, Filename: "log.txt", SizeBytes: 42, StorageKey: "k1"})
		att, _ := s.ListAttachments(ctx, "acme", tk.ID)
		if len(att) != 1 || att[0].SizeBytes != 42 {
			t.Fatalf("attachment metadata mismatch; got %+v", att)
		}
		if wrong, _ := s.ListAttachments(ctx, "globex", tk.ID); len(wrong) != 0 {
			t.Fatal("a different partition must not list attachments")
		}
	})
}

func TestKB_SearchAndCategoryScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		s.CreateCategory(ctx, &KBCategory{Partition: "acme", Name: "Billing", Slug: "billing"})
		s.CreateArticle(ctx, &KBArticle{Partition: "acme", Title: "Refund policy", Slug: "refunds", Content: "how to get a refund", CategorySlug: "billing"})
		s.CreateArticle(ctx, &KBArticle{Partition: "acme", Title: "Login help", Slug: "login", Content: "reset password", CategorySlug: "account"})
		if got, _ := s.SearchArticles(ctx, "acme", "refund"); len(got) != 1 || got[0].Slug != "refunds" {
			t.Fatalf("search must match title/content; got %+v", got)
		}
		if got, _ := s.ListArticles(ctx, "acme", "billing"); len(got) != 1 {
			t.Fatalf("category filter must narrow; got %d", len(got))
		}
		if got, _ := s.SearchArticles(ctx, "globex", "refund"); len(got) != 0 {
			t.Fatal("search is partition-scoped")
		}
		if c, err := s.GetCategoryBySlug(ctx, "acme", "billing"); err != nil || c.Name != "Billing" {
			t.Fatalf("GetCategoryBySlug: %+v %v", c, err)
		}
		a, _ := s.GetArticleBySlug(ctx, "acme", "billing", "refunds")
		s.IncrementArticleViewCount(ctx, a.ID)
		a2, _ := s.GetArticleBySlug(ctx, "acme", "billing", "refunds")
		if a2.ViewCount != 1 {
			t.Fatalf("view count must increment; got %d", a2.ViewCount)
		}
	})
}

// TestResolveTicketPartition_IsTheOnlyWayAcrossPartitions pins the replacement for
// an empty partition meaning "see everything": reach across partitions exists, but
// only through a call that says who is asking and leaves an event behind.
//
// It asserts the two legs produce DIFFERENT outcomes rather than each returning
// what its own case expects. A test that only checks "the call I expected to work
// worked" cannot tell a real filter from one that admits everyone, which is how an
// empty partition passed for an admin in the first place.
func TestResolveTicketPartition_IsTheOnlyWayAcrossPartitions(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		tk := &Ticket{Partition: "acme", UserID: "u1", Email: "a@x", Subject: "help", Status: "open"}
		if err := s.CreateTicket(ctx, tk); err != nil {
			t.Fatalf("CreateTicket: %v", err)
		}
		if err := s.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "user", Content: "hello"}); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}

		// A caller that names no partition reads no messages.
		blind, err := s.ListMessages(ctx, "", tk.ID, true)
		if err != nil || len(blind) != 0 {
			t.Fatalf("an empty partition must read no messages, got %d err=%v", len(blind), err)
		}

		// Unattributed reach is refused, so the event below is never missing.
		for _, missing := range []struct{ actorType, actorID, reason string }{
			{"", "admin-1", "escalation"},
			{"staff", "", "escalation"},
			{"staff", "admin-1", ""},
		} {
			if _, err := s.ResolveTicketPartition(ctx, missing.actorType, missing.actorID, tk.ID, missing.reason); !errors.Is(err, ErrActorRequired) {
				t.Fatalf("resolve(%q,%q,%q) must refuse, got %v", missing.actorType, missing.actorID, missing.reason, err)
			}
		}

		// Attributed, it answers — and that answer is what makes the scoped read work.
		partition, err := s.ResolveTicketPartition(ctx, "staff", "admin-1", tk.ID, "escalation")
		if err != nil || partition != "acme" {
			t.Fatalf("resolve: partition=%q err=%v", partition, err)
		}
		seeing, err := s.ListMessages(ctx, partition, tk.ID, true)
		if err != nil || len(seeing) != 1 {
			t.Fatalf("the resolved partition must read the messages, got %d err=%v", len(seeing), err)
		}
		if len(blind) == len(seeing) {
			t.Fatal("naming no partition and naming the right one produced the same result; the filter admits everyone")
		}

		// The ticket's own partition can see that someone crossed into it.
		events, err := s.ListTicketEvents(ctx, "acme", tk.ID)
		if err != nil {
			t.Fatalf("ListTicketEvents: %v", err)
		}
		var found *TicketEvent
		for _, e := range events {
			if e.EventType == EventCrossPartitionResolve {
				found = e
			}
		}
		if found == nil {
			t.Fatal("a cross-partition resolve left no event; the reach would be unattributable")
		}
		if found.ActorID != "admin-1" || found.Metadata != "escalation" {
			t.Fatalf("event must carry who and why, got actor=%q reason=%q", found.ActorID, found.Metadata)
		}
	})
}
