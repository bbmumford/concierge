package concierge

import (
	"context"
	"errors"
	"testing"
	"time"
)

var ctx = context.Background()

func storeAt(t time.Time) *MemStore {
	m := NewMemStore()
	m.now = func() time.Time { return t }
	return m
}

func TestTicket_CreateGetOwnerScopeAndExternalRef(t *testing.T) {
	m := storeAt(time.Unix(1000, 0))
	tk := &Ticket{Owner: "acme", UserID: "u1", Email: "a@x", Subject: "help", Status: "open"}
	if err := m.CreateTicket(ctx, tk); err != nil || tk.ID == "" || tk.Number == "" {
		t.Fatalf("CreateTicket: %v id=%q", err, tk.ID)
	}
	// owner-scoped get: right owner resolves, wrong owner is ErrNotFound
	if got, err := m.GetTicket(ctx, "acme", tk.ID); err != nil || got.Subject != "help" {
		t.Fatalf("GetTicket acme: %+v %v", got, err)
	}
	if _, err := m.GetTicket(ctx, "globex", tk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a different owner must not see the ticket")
	}
	// empty owner = admin view
	if _, err := m.GetTicket(ctx, "", tk.ID); err != nil {
		t.Fatal("empty owner (admin) must resolve")
	}
	// external-ref link + lookup (channel-agnostic)
	if err := m.SetExternalRef(ctx, tk.ID, "discord.channel", "chan-99"); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetTicketByExternalRef(ctx, "discord.channel", "chan-99")
	if err != nil || got.ID != tk.ID {
		t.Fatalf("GetTicketByExternalRef: %+v %v", got, err)
	}
}

func TestTicket_StatusTimestampsAndBreach(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	m := storeAt(now)
	tk := &Ticket{Owner: "acme", Status: "open", CreatedAt: now.Add(-2 * time.Hour)}
	m.CreateTicket(ctx, tk)
	// resolving stamps ResolvedAt
	if err := m.UpdateTicketStatus(ctx, "acme", tk.ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetTicket(ctx, "acme", tk.ID)
	if got.ResolvedAt == nil {
		t.Fatal("resolving must stamp ResolvedAt")
	}
	// a wrong-owner update is denied
	if err := m.UpdateTicketStatus(ctx, "globex", tk.ID, "closed"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a different owner must not update the ticket")
	}
	// breach: an open unresponded 2h-old ticket breaches a 1h first-response SLA
	open := &Ticket{Owner: "acme", Status: "open", CreatedAt: now.Add(-2 * time.Hour)}
	m.CreateTicket(ctx, open)
	breached, _ := m.ListBreachedTickets(ctx, time.Hour, 0, now)
	found := false
	for _, b := range breached {
		if b.ID == open.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("a 2h-old unresponded open ticket must breach a 1h first-response SLA; got %d breached", len(breached))
	}
}

func TestMessages_InternalFilterAndOwnerGuard(t *testing.T) {
	m := storeAt(time.Unix(1000, 0))
	tk := &Ticket{Owner: "acme", Status: "open"}
	m.CreateTicket(ctx, tk)
	m.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "user", Content: "hi", CreatedAt: time.Unix(1001, 0)})
	m.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "agent", Content: "note", IsInternal: true, CreatedAt: time.Unix(1002, 0)})
	// public view excludes internal
	if pub, _ := m.ListMessages(ctx, "acme", tk.ID, false); len(pub) != 1 || pub[0].Content != "hi" {
		t.Fatalf("public message list must exclude internal notes; got %+v", pub)
	}
	if all, _ := m.ListMessages(ctx, "acme", tk.ID, true); len(all) != 2 {
		t.Fatalf("internal-inclusive list must return both; got %d", len(all))
	}
	// wrong owner sees nothing
	if wrong, _ := m.ListMessages(ctx, "globex", tk.ID, true); len(wrong) != 0 {
		t.Fatal("a different owner must not see a ticket's messages")
	}
	// since filter
	if since, _ := m.ListMessagesSince(ctx, "acme", tk.ID, time.Unix(1001, 0), true); len(since) != 1 {
		t.Fatalf("ListMessagesSince must exclude messages at/before `since`; got %d", len(since))
	}
}

func TestEvents_AppendOnlyAudit(t *testing.T) {
	m := storeAt(time.Unix(1000, 0))
	tk := &Ticket{Owner: "acme", Status: "open"}
	m.CreateTicket(ctx, tk)
	m.AddTicketEvent(ctx, &TicketEvent{TicketID: tk.ID, EventType: "status_change", OldValue: "open", NewValue: "resolved", CreatedAt: time.Unix(1001, 0)})
	ev, _ := m.ListTicketEvents(ctx, "acme", tk.ID)
	if len(ev) != 1 || ev[0].NewValue != "resolved" {
		t.Fatalf("event audit must record the transition; got %+v", ev)
	}
}

func TestKB_SearchAndCategoryScope(t *testing.T) {
	m := storeAt(time.Unix(1000, 0))
	m.CreateCategory(ctx, &KBCategory{Owner: "acme", Name: "Billing", Slug: "billing"})
	m.CreateArticle(ctx, &KBArticle{Owner: "acme", Title: "Refund policy", Slug: "refunds", Content: "how to get a refund", CategorySlug: "billing"})
	m.CreateArticle(ctx, &KBArticle{Owner: "acme", Title: "Login help", Slug: "login", Content: "reset password", CategorySlug: "account"})
	if got, _ := m.SearchArticles(ctx, "acme", "refund"); len(got) != 1 || got[0].Slug != "refunds" {
		t.Fatalf("search must match title/content; got %+v", got)
	}
	if got, _ := m.ListArticles(ctx, "acme", "billing"); len(got) != 1 {
		t.Fatalf("category filter must narrow; got %d", len(got))
	}
	if got, _ := m.SearchArticles(ctx, "globex", "refund"); len(got) != 0 {
		t.Fatal("search is owner-scoped")
	}
	a, _ := m.GetArticleBySlug(ctx, "acme", "billing", "refunds")
	m.IncrementArticleViewCount(ctx, a.ID)
	a2, _ := m.GetArticleBySlug(ctx, "acme", "billing", "refunds")
	if a2.ViewCount != 1 {
		t.Fatalf("view count must increment; got %d", a2.ViewCount)
	}
}
