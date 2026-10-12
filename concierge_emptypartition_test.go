package concierge

import (
	"errors"
	"testing"
	"time"
)

// An empty partition names no partition, on both stores: every operation that is
// confined to the partition it is given answers nothing, or ErrNotFound, when the
// partition it is given is empty. The rows it could wrongly reach are stored under
// the empty partition itself, which CreateTicket, CreateCategory and CreateArticle
// accept, so a store that dropped the "is the partition set" half of a filter and
// kept only "partition = ?" would answer every one of these calls with a row.
//
// Each read is checked beside a second partition's rows, so a read that returned
// everything is caught as well as one that matched the empty partition.
func TestAnEmptyPartitionReachesNothing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		blank := &Ticket{ID: "tk-blank-partition", Partition: "", UserID: "u1", Email: "a@example.com", GroupID: "g1", Status: "open", Priority: "normal"}
		named := &Ticket{ID: "tk-named-partition", Partition: "acme", UserID: "u1", Email: "a@example.com", GroupID: "g1", Status: "open", Priority: "normal"}
		for _, tk := range []*Ticket{blank, named} {
			if err := s.CreateTicket(ctx, tk); err != nil {
				t.Fatalf("CreateTicket %s: %v", tk.ID, err)
			}
			if err := s.AddMessage(ctx, &TicketMessage{TicketID: tk.ID, AuthorType: "user", Content: "hi", CreatedAt: time.Unix(1001, 0)}); err != nil {
				t.Fatalf("AddMessage %s: %v", tk.ID, err)
			}
			if err := s.AddTicketEvent(ctx, &TicketEvent{TicketID: tk.ID, EventType: "status_change", NewValue: "open", CreatedAt: time.Unix(1001, 0)}); err != nil {
				t.Fatalf("AddTicketEvent %s: %v", tk.ID, err)
			}
			if err := s.AddAttachment(ctx, &Attachment{TicketID: tk.ID, Filename: "log.txt", SizeBytes: 42, StorageKey: "k-" + tk.ID}); err != nil {
				t.Fatalf("AddAttachment %s: %v", tk.ID, err)
			}
		}
		for _, partition := range []string{"", "acme"} {
			if err := s.CreateCategory(ctx, &KBCategory{Partition: partition, Name: "Billing", Slug: "billing"}); err != nil {
				t.Fatalf("CreateCategory under %q: %v", partition, err)
			}
			if err := s.CreateArticle(ctx, &KBArticle{Partition: partition, Title: "Refund policy", Slug: "refunds", Content: "how to get a refund", CategorySlug: "billing", Status: ArticleStatusPublished}); err != nil {
				t.Fatalf("CreateArticle under %q: %v", partition, err)
			}
		}

		// Tickets.
		if got, err := s.GetTicket(ctx, "", blank.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetTicket with the empty partition: %+v, err=%v, want ErrNotFound", got, err)
		}
		if got, err := s.GetTicket(ctx, "", named.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetTicket with the empty partition, for another partition's ticket: %+v, err=%v, want ErrNotFound", got, err)
		}
		if got, err := s.ListTicketsByUser(ctx, "", "u1", ""); err != nil || len(got) != 0 {
			t.Errorf("ListTicketsByUser with the empty partition: %d tickets, err=%v, want none", len(got), err)
		}
		if got, err := s.ListTicketsByEmail(ctx, "", "a@example.com"); err != nil || len(got) != 0 {
			t.Errorf("ListTicketsByEmail with the empty partition: %d tickets, err=%v, want none", len(got), err)
		}
		if got, err := s.ListTicketsByGroup(ctx, "", "g1", ""); err != nil || len(got) != 0 {
			t.Errorf("ListTicketsByGroup with the empty partition: %d tickets, err=%v, want none", len(got), err)
		}
		for _, userID := range []string{"", "u1"} {
			if got, err := s.GetTicketStats(ctx, "", userID); err != nil || got == nil || *got != (TicketStats{}) {
				t.Errorf("GetTicketStats with the empty partition, user %q: %+v, err=%v, want every count zero", userID, got, err)
			}
		}

		// The two writes that take a partition.
		for _, id := range []string{blank.ID, named.ID} {
			if err := s.UpdateTicketStatus(ctx, "", id, "closed"); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateTicketStatus with the empty partition on %s: err=%v, want ErrNotFound", id, err)
			}
			if err := s.UpdateTicketPriority(ctx, "", id, "urgent"); !errors.Is(err, ErrNotFound) {
				t.Errorf("UpdateTicketPriority with the empty partition on %s: err=%v, want ErrNotFound", id, err)
			}
		}
		all, total, err := s.ListAllTickets(ctx, "agent", "a1", "checking a refused write changed nothing", "", "", 10, 0)
		if err != nil || total != 2 || len(all) != 2 {
			t.Fatalf("ListAllTickets: %d of %d, err=%v, want both tickets", len(all), total, err)
		}
		for _, tk := range all {
			if tk.Status != "open" || tk.Priority != "normal" || tk.ClosedAt != nil {
				t.Errorf("a write refused for the empty partition changed %s: status %q, priority %q, closed %v", tk.ID, tk.Status, tk.Priority, tk.ClosedAt)
			}
		}

		// A ticket's children.
		for _, id := range []string{blank.ID, named.ID} {
			if got, err := s.ListMessages(ctx, "", id, true); err != nil || len(got) != 0 {
				t.Errorf("ListMessages with the empty partition on %s: %d messages, err=%v, want none", id, len(got), err)
			}
			if got, err := s.ListMessagesSince(ctx, "", id, time.Unix(0, 0), true); err != nil || len(got) != 0 {
				t.Errorf("ListMessagesSince with the empty partition on %s: %d messages, err=%v, want none", id, len(got), err)
			}
			if got, err := s.ListTicketEvents(ctx, "", id); err != nil || len(got) != 0 {
				t.Errorf("ListTicketEvents with the empty partition on %s: %d events, err=%v, want none", id, len(got), err)
			}
			if got, err := s.ListAttachments(ctx, "", id); err != nil || len(got) != 0 {
				t.Errorf("ListAttachments with the empty partition on %s: %d attachments, err=%v, want none", id, len(got), err)
			}
		}

		// The knowledge base.
		if got, err := s.GetCategoryBySlug(ctx, "", "billing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetCategoryBySlug with the empty partition: %+v, err=%v, want ErrNotFound", got, err)
		}
		if got, err := s.ListCategories(ctx, ""); err != nil || len(got) != 0 {
			t.Errorf("ListCategories with the empty partition: %d categories, err=%v, want none", len(got), err)
		}
		for _, category := range []string{"", "billing"} {
			if got, err := s.GetArticleBySlug(ctx, "", category, "refunds"); !errors.Is(err, ErrNotFound) {
				t.Errorf("GetArticleBySlug with the empty partition, category %q: %+v, err=%v, want ErrNotFound", category, got, err)
			}
			if got, err := s.ListArticles(ctx, "", category); err != nil || len(got) != 0 {
				t.Errorf("ListArticles with the empty partition, category %q: %d articles, err=%v, want none", category, len(got), err)
			}
		}
		if got, err := s.SearchArticles(ctx, "", "refund"); err != nil || len(got) != 0 {
			t.Errorf("SearchArticles with the empty partition: %d articles, err=%v, want none", len(got), err)
		}

		// The named partition's own rows answer, so the checks above are of the empty
		// partition and not of a store that answers nothing at all.
		if got, err := s.GetTicket(ctx, "acme", named.ID); err != nil || got.ID != named.ID {
			t.Errorf("GetTicket under its own partition: %+v, err=%v", got, err)
		}
		if got, err := s.ListArticles(ctx, "acme", "billing"); err != nil || len(got) != 1 {
			t.Errorf("ListArticles under its own partition: %d articles, err=%v, want 1", len(got), err)
		}
		if got, err := s.ListMessages(ctx, "acme", named.ID, true); err != nil || len(got) != 1 {
			t.Errorf("ListMessages under its own partition: %d messages, err=%v, want 1", len(got), err)
		}
	})
}
