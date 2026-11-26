package concierge

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// fullStore is both halves, as MemStore and SQLStore each are.
type fullStore interface {
	Store
	DeskStore
}

// eachDeskStore runs a case against MemStore and SQLStore, so the SQL store is
// held to the semantics the reference defines for the desk half too.
func eachDeskStore(t *testing.T, fn func(t *testing.T, s fullStore)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })
	t.Run("sql", func(t *testing.T) {
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
			t.Fatalf("InitSchema: %v", err)
		}
		// A host runs InitSchema at every boot over the same database.
		if err := s.InitSchema(ctx); err != nil {
			t.Fatalf("second InitSchema: %v", err)
		}
		fn(t, s)
	})
}

func mustTicket(t *testing.T, s Store, partition, status string, created time.Time) *Ticket {
	t.Helper()
	tk := &Ticket{Partition: partition, Subject: "help", Status: status, CreatedAt: created}
	if err := s.CreateTicket(ctx, tk); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	return tk
}

func TestDesk_ContactsArePartitionScoped(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		if err := s.CreateContact(ctx, &Contact{Name: "x"}); !errors.Is(err, ErrEmptyPartition) {
			t.Fatalf("empty partition: %v", err)
		}
		c := &Contact{Partition: "acme", Name: "Ann", Email: "ann@acme.test", Metadata: map[string]string{"plan": "pro"}}
		if err := s.CreateContact(ctx, c); err != nil || c.ID == "" {
			t.Fatalf("CreateContact: %v id=%q", err, c.ID)
		}
		if err := s.CreateContact(ctx, &Contact{ID: c.ID, Partition: "other"}); !errors.Is(err, ErrDuplicateID) {
			t.Fatalf("held id: %v", err)
		}
		got, err := s.GetContact(ctx, "acme", c.ID)
		if err != nil || got.Email != "ann@acme.test" || got.Metadata["plan"] != "pro" {
			t.Fatalf("GetContact: %+v, %v", got, err)
		}
		if _, err := s.GetContact(ctx, "other", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wrong partition: %v", err)
		}
		if _, err := s.GetContact(ctx, "", c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty partition read: %v", err)
		}
		c.Name, c.AccountID = "Ann B", "acct-1"
		if err := s.UpdateContact(ctx, c); err != nil {
			t.Fatalf("UpdateContact: %v", err)
		}
		if got, _ := s.GetContact(ctx, "acme", c.ID); got.Name != "Ann B" || got.AccountID != "acct-1" {
			t.Fatalf("after update: %+v", got)
		}
		if err := s.UpdateContact(ctx, &Contact{ID: c.ID, Partition: "other"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("update in another partition: %v", err)
		}
	})
}

func TestDesk_OneChannelIdentityNamesOneContact(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		ann := &Contact{Partition: "acme", Name: "Ann"}
		bob := &Contact{Partition: "acme", Name: "Bob"}
		for _, c := range []*Contact{ann, bob} {
			if err := s.CreateContact(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		id := &ContactIdentity{ContactID: ann.ID, Partition: "acme", Channel: "email", ExternalID: "ann@acme.test"}
		if err := s.LinkIdentity(ctx, id); err != nil {
			t.Fatalf("LinkIdentity: %v", err)
		}
		// The same link again is a no-op; a verified one stamps it.
		verified := time.Unix(1_800_000_000, 0).UTC()
		if err := s.LinkIdentity(ctx, &ContactIdentity{ContactID: ann.ID, Partition: "acme", Channel: "email", ExternalID: "ann@acme.test", VerifiedAt: &verified}); err != nil {
			t.Fatalf("relink: %v", err)
		}
		if err := s.LinkIdentity(ctx, &ContactIdentity{ContactID: bob.ID, Partition: "acme", Channel: "email", ExternalID: "ann@acme.test"}); !errors.Is(err, ErrIdentityTaken) {
			t.Fatalf("taken identity: %v", err)
		}
		got, err := s.FindContactByIdentity(ctx, "acme", "email", "ann@acme.test")
		if err != nil || got.ID != ann.ID {
			t.Fatalf("FindContactByIdentity: %+v, %v", got, err)
		}
		ids, err := s.ListContactIdentities(ctx, "acme", ann.ID)
		if err != nil || len(ids) != 1 || ids[0].VerifiedAt == nil || !ids[0].VerifiedAt.Equal(verified) {
			t.Fatalf("ListContactIdentities: %+v, %v", ids, err)
		}
		// Another partition holds its own contact under the same identity.
		other := &Contact{Partition: "beta"}
		if err := s.CreateContact(ctx, other); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkIdentity(ctx, &ContactIdentity{ContactID: other.ID, Partition: "beta", Channel: "email", ExternalID: "ann@acme.test"}); err != nil {
			t.Fatalf("same identity in another partition: %v", err)
		}
		if got, _ := s.FindContactByIdentity(ctx, "beta", "email", "ann@acme.test"); got == nil || got.ID != other.ID {
			t.Fatalf("beta's contact: %+v", got)
		}
		if _, err := s.FindContactByIdentity(ctx, "", "email", "ann@acme.test"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty partition find: %v", err)
		}
		// A contact the partition does not hold cannot be linked.
		if err := s.LinkIdentity(ctx, &ContactIdentity{ContactID: ann.ID, Partition: "beta", Channel: "slack.user", ExternalID: "T1/U1"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("link to a contact in another partition: %v", err)
		}
	})
}

func TestDesk_ClaimThreadRefRecognisesARepeatedDelivery(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		if _, err := s.ClaimThreadRef(ctx, &ThreadRef{Channel: "chat.conversation"}); !errors.Is(err, ErrInvalidThreadRef) {
			t.Fatalf("invalid ref: %v", err)
		}
		first := &ThreadRef{Channel: "chat.conversation", ExternalID: "1/42", Partition: "help", TicketID: "t-1", ContactID: "c-1"}
		claimed, err := s.ClaimThreadRef(ctx, first)
		if err != nil || !claimed {
			t.Fatalf("first claim: %v %v", claimed, err)
		}
		again := &ThreadRef{Channel: "chat.conversation", ExternalID: "1/42", Partition: "help", TicketID: "t-2"}
		claimed, err = s.ClaimThreadRef(ctx, again)
		if err != nil || claimed || again.TicketID != "t-1" || again.ContactID != "c-1" {
			t.Fatalf("second claim: %v %v %+v", claimed, err, again)
		}
		got, err := s.GetThreadRef(ctx, "chat.conversation", "1/42")
		if err != nil || got.TicketID != "t-1" {
			t.Fatalf("GetThreadRef: %+v, %v", got, err)
		}
		if _, err := s.GetThreadRef(ctx, "chat.conversation", "1/43"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown ref: %v", err)
		}
		// The same external id on another channel is another ref.
		if claimed, err := s.ClaimThreadRef(ctx, &ThreadRef{Channel: "chat.message", ExternalID: "1/42", Partition: "help", TicketID: "t-1", MessageID: "m-1"}); err != nil || !claimed {
			t.Fatalf("other channel: %v %v", claimed, err)
		}
	})
}

func TestDesk_ConcurrentClaimsHaveOneWinner(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		const n = 8
		var wg sync.WaitGroup
		wins := make(chan string, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := &ThreadRef{Channel: "chat.message", ExternalID: "1/7", Partition: "help", TicketID: string(rune('a' + i))}
				claimed, err := s.ClaimThreadRef(ctx, r)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if claimed {
					wins <- r.TicketID
				}
			}(i)
		}
		wg.Wait()
		close(wins)
		var won []string
		for w := range wins {
			won = append(won, w)
		}
		if len(won) != 1 {
			t.Fatalf("winners = %v; want exactly one", won)
		}
	})
}

func TestDesk_AssignRoutesAndRecordsTheChange(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		team := &Team{Partition: "desk", Slug: "tier1", Name: "Tier 1", Members: []string{"u1", "u2"}}
		if err := s.UpsertTeam(ctx, team); err != nil || team.ID == "" {
			t.Fatalf("UpsertTeam: %v", err)
		}
		firstID := team.ID
		team2 := &Team{Partition: "desk", Slug: "tier1", Name: "Tier One", Members: []string{"u3"}}
		if err := s.UpsertTeam(ctx, team2); err != nil || team2.ID != firstID {
			t.Fatalf("re-upsert kept id: %v %q %q", err, team2.ID, firstID)
		}
		teams, err := s.ListTeams(ctx, "desk")
		if err != nil || len(teams) != 1 || teams[0].Name != "Tier One" || len(teams[0].Members) != 1 || teams[0].Members[0] != "u3" {
			t.Fatalf("ListTeams: %+v, %v", teams, err)
		}
		q := &Queue{Partition: "desk", Slug: "general", Name: "General", TeamID: team.ID}
		if err := s.UpsertQueue(ctx, q); err != nil || q.ID == "" {
			t.Fatalf("UpsertQueue: %v", err)
		}
		if qs, _ := s.ListQueues(ctx, "desk"); len(qs) != 1 || qs[0].TeamID != team.ID {
			t.Fatalf("ListQueues: %+v", qs)
		}
		if qs, _ := s.ListQueues(ctx, ""); len(qs) != 0 {
			t.Fatalf("empty partition ListQueues: %+v", qs)
		}

		tk := mustTicket(t, s, "acme", "open", time.Time{})
		if err := s.Assign(ctx, &Assignment{TicketID: tk.ID, Partition: "beta", QueueID: q.ID}, "operator", "op1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("assign in another partition: %v", err)
		}
		if err := s.Assign(ctx, &Assignment{TicketID: tk.ID, Partition: "acme", QueueID: "nope"}, "operator", "op1"); !errors.Is(err, ErrUnknownRoute) {
			t.Fatalf("unknown queue: %v", err)
		}
		if err := s.Assign(ctx, &Assignment{TicketID: tk.ID, Partition: "acme", QueueID: q.ID, TeamID: team.ID}, "operator", "op1"); err != nil {
			t.Fatalf("Assign: %v", err)
		}
		if err := s.Assign(ctx, &Assignment{TicketID: tk.ID, Partition: "acme", QueueID: q.ID, TeamID: team.ID, AssigneeID: "u3"}, "operator", "op2"); err != nil {
			t.Fatalf("reassign: %v", err)
		}
		a, err := s.GetAssignment(ctx, "acme", tk.ID)
		if err != nil || a.AssigneeID != "u3" || a.UpdatedBy != "op2" {
			t.Fatalf("GetAssignment: %+v, %v", a, err)
		}
		if _, err := s.GetAssignment(ctx, "beta", tk.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("assignment from another partition: %v", err)
		}
		events, _ := s.ListTicketEvents(ctx, "acme", tk.ID)
		var changes []*TicketEvent
		for _, e := range events {
			if e.EventType == EventAssignment {
				changes = append(changes, e)
			}
		}
		if len(changes) != 2 || changes[0].OldValue != "//" || changes[1].OldValue != q.ID+"/"+team.ID+"/" ||
			changes[1].NewValue != q.ID+"/"+team.ID+"/u3" || changes[1].ActorID != "op2" {
			t.Fatalf("assignment events: %+v", changes)
		}
	})
}

func TestDesk_ListQueueTicketsCrossesPartitionsOnTheRecord(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		q := &Queue{Partition: "desk", Slug: "general"}
		if err := s.UpsertQueue(ctx, q); err != nil {
			t.Fatal(err)
		}
		base := time.Unix(1_800_000_000, 0).UTC()
		older := mustTicket(t, s, "acme", "open", base)
		newer := mustTicket(t, s, "beta", "open", base.Add(time.Hour))
		done := mustTicket(t, s, "acme", "resolved", base.Add(-time.Hour))
		unqueued := mustTicket(t, s, "acme", "open", base)
		_ = unqueued
		for _, tk := range []*Ticket{older, newer, done} {
			if err := s.Assign(ctx, &Assignment{TicketID: tk.ID, Partition: tk.Partition, QueueID: q.ID}, "operator", "op"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.ListQueueTickets(ctx, "", "op", "why", q.ID, 10, 0); !errors.Is(err, ErrActorRequired) {
			t.Fatalf("no actor: %v", err)
		}
		if _, err := s.ListQueueTickets(ctx, "operator", "op", "why", q.ID, 0, 0); !errors.Is(err, ErrUnboundedAdminList) {
			t.Fatalf("no limit: %v", err)
		}
		got, err := s.ListQueueTickets(ctx, "operator", "op", "triage", q.ID, 10, 0)
		if err != nil || len(got) != 2 || got[0].ID != older.ID || got[1].ID != newer.ID {
			t.Fatalf("ListQueueTickets: %v %v", ids(got), err)
		}
		page, _ := s.ListQueueTickets(ctx, "operator", "op", "triage", q.ID, 1, 1)
		if len(page) != 1 || page[0].ID != newer.ID {
			t.Fatalf("second page: %v", ids(page))
		}
		events, _ := s.ListTicketEvents(ctx, "beta", newer.ID)
		var seen int
		for _, e := range events {
			if e.EventType == EventCrossPartitionQueue && e.ActorID == "op" && e.Metadata == "triage" {
				seen++
			}
		}
		if seen != 2 {
			t.Fatalf("disclosure events on beta's ticket = %d; want 2", seen)
		}
	})
}

func ids(ts []*Ticket) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestDesk_SLAStartsPausesAndReportsBreaches(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		if err := s.UpsertSLAPolicy(ctx, &SLAPolicy{Partition: "desk", Slug: "x", FirstResponse: -time.Second}); !errors.Is(err, ErrInvalidSLAPolicy) {
			t.Fatalf("negative target: %v", err)
		}
		hours := officeHours()
		hours.TimeZone = "UTC"
		p := &SLAPolicy{Partition: "desk", Slug: "standard", Name: "Standard", FirstResponse: time.Hour, Resolution: 8 * time.Hour,
			Hours: hours, PauseStatuses: []string{"waiting"}}
		if err := s.UpsertSLAPolicy(ctx, p); err != nil || p.ID == "" {
			t.Fatalf("UpsertSLAPolicy: %v", err)
		}
		got, err := s.GetSLAPolicy(ctx, p.ID)
		if err != nil || got.Hours.Week[time.Monday][0].End != 17*60 || got.PauseStatuses[0] != "waiting" || got.Resolution != 8*time.Hour {
			t.Fatalf("GetSLAPolicy: %+v, %v", got, err)
		}
		if ps, _ := s.ListSLAPolicies(ctx, "desk"); len(ps) != 1 {
			t.Fatalf("ListSLAPolicies: %+v", ps)
		}

		monday := time.Date(2026, 11, 30, 9, 0, 0, 0, time.UTC)
		tk := mustTicket(t, s, "acme", "open", monday)
		if _, err := s.GetTicketSLA(ctx, "acme", tk.ID); !errors.Is(err, ErrSLANotStarted) {
			t.Fatalf("before start: %v", err)
		}
		if err := s.StartSLA(ctx, "acme", tk.ID, "nope", monday); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown policy: %v", err)
		}
		if err := s.StartSLA(ctx, "beta", tk.ID, p.ID, monday); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ticket in another partition: %v", err)
		}
		if err := s.StartSLA(ctx, "acme", tk.ID, p.ID, monday); err != nil {
			t.Fatalf("StartSLA: %v", err)
		}
		if err := s.PauseSLA(ctx, "acme", tk.ID, monday.Add(30*time.Minute)); err != nil {
			t.Fatalf("PauseSLA: %v", err)
		}
		// A second pause keeps the first; a resume closes it; a second resume is a no-op.
		if err := s.PauseSLA(ctx, "acme", tk.ID, monday.Add(40*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.ResumeSLA(ctx, "acme", tk.ID, monday.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := s.ResumeSLA(ctx, "acme", tk.ID, monday.Add(4*time.Hour)); err != nil {
			t.Fatal(err)
		}
		state, err := s.GetTicketSLA(ctx, "acme", tk.ID)
		if err != nil || len(state.Pauses) != 1 || !state.Pauses[0].From.Equal(monday.Add(30*time.Minute)) ||
			state.Pauses[0].To == nil || !state.Pauses[0].To.Equal(monday.Add(3*time.Hour)) || state.Paused() {
			t.Fatalf("GetTicketSLA: %+v, %v", state, err)
		}

		// At 12:15 the clock has run 30 minutes before the pause and 15 after.
		if br, _ := s.ListSLABreaches(ctx, "operator", "op", "sla", monday.Add(3*time.Hour+15*time.Minute), 10); len(br) != 0 {
			t.Fatalf("breaches at 12:15: %v", ids(br))
		}
		br, err := s.ListSLABreaches(ctx, "operator", "op", "sla", monday.Add(3*time.Hour+45*time.Minute), 10)
		if err != nil || len(br) != 1 || br[0].ID != tk.ID {
			t.Fatalf("breaches at 12:45: %v %v", ids(br), err)
		}
		if _, err := s.ListSLABreaches(ctx, "operator", "", "sla", monday, 10); !errors.Is(err, ErrActorRequired) {
			t.Fatalf("no actor: %v", err)
		}
		events, _ := s.ListTicketEvents(ctx, "acme", tk.ID)
		var seen bool
		for _, e := range events {
			seen = seen || (e.EventType == EventCrossPartitionQueue && e.Metadata == "sla")
		}
		if !seen {
			t.Fatal("ListSLABreaches wrote no disclosure event")
		}
		// A first response stamped in time ends the first-response leg.
		if err := s.SetFirstResponseAt(ctx, tk.ID, monday.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if br, _ := s.ListSLABreaches(ctx, "operator", "op", "sla", monday.Add(3*time.Hour+45*time.Minute), 10); len(br) != 0 {
			t.Fatalf("breaches after the first response: %v", ids(br))
		}
	})
}

func TestDesk_RatingsOnePerTicket(t *testing.T) {
	eachDeskStore(t, func(t *testing.T, s fullStore) {
		tk := mustTicket(t, s, "acme", "resolved", time.Time{})
		for _, bad := range []*Rating{{TicketID: tk.ID, Partition: "acme", Score: 0}, {TicketID: tk.ID, Partition: "acme", Score: 6}, {Partition: "acme", Score: 3}} {
			if err := s.RecordRating(ctx, bad); !errors.Is(err, ErrInvalidRating) {
				t.Fatalf("RecordRating(%+v) = %v; want ErrInvalidRating", bad, err)
			}
		}
		if err := s.RecordRating(ctx, &Rating{TicketID: tk.ID, Partition: "beta", Score: 4}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rating a ticket in another partition: %v", err)
		}
		at := time.Unix(1_800_000_000, 0).UTC()
		r := &Rating{TicketID: tk.ID, Partition: "acme", Score: 2, Comment: "slow", RatedBy: "c-1", Source: "chat", CreatedAt: at}
		if err := s.RecordRating(ctx, r); err != nil || r.ID == "" {
			t.Fatalf("RecordRating: %v", err)
		}
		firstID := r.ID
		again := &Rating{TicketID: tk.ID, Partition: "acme", Score: 4, Comment: "sorted"}
		if err := s.RecordRating(ctx, again); err != nil || again.ID != firstID || !again.CreatedAt.Equal(at) {
			t.Fatalf("re-rate: %v %+v", err, again)
		}
		got, err := s.GetRating(ctx, "acme", tk.ID)
		if err != nil || got.Score != 4 || got.Comment != "sorted" || got.ID != firstID {
			t.Fatalf("GetRating: %+v, %v", got, err)
		}
		tk2 := mustTicket(t, s, "acme", "resolved", time.Time{})
		if err := s.RecordRating(ctx, &Rating{TicketID: tk2.ID, Partition: "acme", Score: 5, CreatedAt: at.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		sum, err := s.SummariseRatings(ctx, "acme", time.Time{})
		if err != nil || sum.Count != 2 || sum.Average != 4.5 || sum.Distribution[4] != 1 || sum.Distribution[5] != 1 {
			t.Fatalf("SummariseRatings: %+v, %v", sum, err)
		}
		if sum, _ := s.SummariseRatings(ctx, "acme", at.Add(30*time.Minute)); sum.Count != 1 || sum.Distribution[5] != 1 {
			t.Fatalf("since: %+v", sum)
		}
		if sum, _ := s.SummariseRatings(ctx, "", time.Time{}); sum.Count != 0 {
			t.Fatalf("empty partition: %+v", sum)
		}
	})
}
