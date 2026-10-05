package concierge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemStore is the in-memory reference Store — the test/dev backend and the semantic reference a
// durable (*sql.DB) store must match. Thread-safe; clock and id generator injectable. Partition is
// opaque: the store filters on it and never reads it. An empty partition on a partition-scoped read
// matches nothing, the same as SQLStore, because a test written against this store is what certifies
// the other's behaviour.
type MemStore struct {
	mu          sync.Mutex
	tickets     map[string]*Ticket
	messages    []*TicketMessage
	events      []*TicketEvent
	attachments []*Attachment
	categories  map[string]*KBCategory
	articles    map[string]*KBArticle
	now         func() time.Time
	newID       func() string
}

// NewMemStore builds an empty help-desk store.
func NewMemStore() *MemStore {
	return &MemStore{
		tickets:    map[string]*Ticket{},
		categories: map[string]*KBCategory{},
		articles:   map[string]*KBArticle{},
		now:        time.Now,
		newID:      randomID,
	}
}

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// partitionMatch reports whether a row stored under partition `have` is visible to
// a caller asking for partition `want`. Every ticket and knowledge-base lookup in
// this store filters through it.
//
// An empty `want` names no partition and matches nothing. SQLStore answers the
// same way — it binds the partition straight into the WHERE clause, where an empty
// string matches only rows stored under an empty partition — and the two have to
// agree, because tests written against this store are what certify the other's
// behaviour.
func partitionMatch(want, have string) bool { return want != "" && want == have }

// ── Tickets ──

func (m *MemStore) CreateTicket(_ context.Context, t *Ticket) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.ID == "" {
		t.ID = m.newID()
	}
	if t.Number == "" {
		t.Number = t.ID[:8]
	}
	now := m.now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	m.tickets[t.ID] = cloneTicket(t)
	return nil
}

func (m *MemStore) GetTicket(_ context.Context, partition, id string) (*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok || !partitionMatch(partition, t.Partition) {
		return nil, ErrNotFound
	}
	return cloneTicket(t), nil
}

func (m *MemStore) GetTicketByExternalRef(_ context.Context, refKey, refValue string) (*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tickets {
		if t.ExternalRefs[refKey] == refValue && refValue != "" {
			return cloneTicket(t), nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) filterTickets(pred func(*Ticket) bool) []*Ticket {
	var out []*Ticket
	for _, t := range m.tickets {
		if pred(t) {
			out = append(out, cloneTicket(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func statusMatch(filter, have string) bool { return filter == "" || filter == have }

func (m *MemStore) ListTicketsByUser(_ context.Context, partition, userID, statusFilter string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return partitionMatch(partition, t.Partition) && t.UserID == userID && statusMatch(statusFilter, t.Status)
	}), nil
}

func (m *MemStore) ListTicketsByEmail(_ context.Context, partition, email string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return partitionMatch(partition, t.Partition) && strings.EqualFold(t.Email, email)
	}), nil
}

func (m *MemStore) ListTicketsByGroup(_ context.Context, partition, groupID, statusFilter string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return partitionMatch(partition, t.Partition) && t.GroupID == groupID && statusMatch(statusFilter, t.Status)
	}), nil
}

func (m *MemStore) ListAllTickets(ctx context.Context, actorType, actorID, reason, statusFilter, categoryFilter string, limit, offset int) ([]*Ticket, int, error) {
	if actorType == "" || actorID == "" || reason == "" {
		return nil, 0, ErrActorRequired
	}
	if limit <= 0 {
		return nil, 0, ErrUnboundedAdminList
	}
	m.mu.Lock()
	all := m.filterTickets(func(t *Ticket) bool {
		return statusMatch(statusFilter, t.Status) && (categoryFilter == "" || t.Category == categoryFilter)
	})
	total := len(all)
	if offset >= total {
		m.mu.Unlock()
		return nil, total, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	m.mu.Unlock()

	// Written before the page is returned, and one per ticket, so a partition's own
	// event log names every cross-partition read that reached one of its tickets.
	// AddTicketEvent takes the lock itself, which is why the page is assembled above
	// and the lock released first.
	for _, t := range all {
		if err := m.AddTicketEvent(ctx, &TicketEvent{
			TicketID:  t.ID,
			EventType: EventCrossPartitionList,
			ActorType: actorType,
			ActorID:   actorID,
			NewValue:  t.Partition,
			Metadata:  reason,
		}); err != nil {
			return nil, 0, err
		}
	}
	return all, total, nil
}

func (m *MemStore) ListBreachedTickets(_ context.Context, firstResponse, resolution time.Duration, now time.Time) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		if t.ClosedAt != nil {
			return false
		}
		age := now.Sub(t.CreatedAt)
		frBreach := t.FirstResponseAt == nil && firstResponse > 0 && age > firstResponse
		resBreach := t.ResolvedAt == nil && resolution > 0 && age > resolution
		return frBreach || resBreach
	}), nil
}

func (m *MemStore) mutateTicket(partition, id string, fn func(*Ticket)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok || !partitionMatch(partition, t.Partition) {
		return ErrNotFound
	}
	fn(t)
	t.UpdatedAt = m.now()
	return nil
}

// mutateTicketUnscoped edits a ticket without a partition filter. Only the two
// Store methods that take no partition use it — SetFirstResponseAt, stamping an
// SLA clock, and SetExternalRef, linking a channel ref the ticket's own partition
// did not supply. Both act on a ticket the caller already identified rather than
// searching for one, so neither widens what a caller can see.
//
// It is a separate method because mutateTicket's filter now refuses an empty
// partition: routing these through it with "" would make an unset partition mean
// "every partition" again, which is the property being removed.
func (m *MemStore) mutateTicketUnscoped(id string, fn func(*Ticket)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok {
		return ErrNotFound
	}
	fn(t)
	t.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) UpdateTicketStatus(_ context.Context, partition, id, status string) error {
	return m.mutateTicket(partition, id, func(t *Ticket) {
		t.Status = status
		if status == "resolved" && t.ResolvedAt == nil {
			n := m.now()
			t.ResolvedAt = &n
		}
		if status == "closed" && t.ClosedAt == nil {
			n := m.now()
			t.ClosedAt = &n
		}
	})
}

func (m *MemStore) UpdateTicketPriority(_ context.Context, partition, id, priority string) error {
	return m.mutateTicket(partition, id, func(t *Ticket) { t.Priority = priority })
}

func (m *MemStore) SetFirstResponseAt(_ context.Context, id string, at time.Time) error {
	return m.mutateTicketUnscoped(id, func(t *Ticket) {
		if t.FirstResponseAt == nil {
			a := at
			t.FirstResponseAt = &a
		}
	})
}

func (m *MemStore) SetExternalRef(_ context.Context, ticketID, refKey, refValue string) error {
	return m.mutateTicketUnscoped(ticketID, func(t *Ticket) {
		if t.ExternalRefs == nil {
			t.ExternalRefs = map[string]string{}
		}
		if refValue == "" {
			delete(t.ExternalRefs, refKey)
		} else {
			t.ExternalRefs[refKey] = refValue
		}
	})
}

// ResolveTicketPartition names the partition a ticket lives under and records the
// request as a ticket event before answering, so the partition that owns the
// ticket can see who looked. It is the one read here not confined to a single
// partition; callers take the partition it returns and use the ordinary scoped
// reads, which is what keeps the reach to a single attributable step.
//
// actorType, actorID and reason are required — an unattributed privileged read is
// the thing this replaces, so it refuses rather than answering anonymously.
func (m *MemStore) ResolveTicketPartition(_ context.Context, actorType, actorID, ticketID, reason string) (string, error) {
	if actorType == "" || actorID == "" || reason == "" {
		return "", ErrActorRequired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[ticketID]
	if !ok {
		return "", ErrNotFound
	}
	m.events = append(m.events, &TicketEvent{
		ID:        m.newID(),
		TicketID:  ticketID,
		EventType: EventCrossPartitionResolve,
		ActorType: actorType,
		ActorID:   actorID,
		NewValue:  t.Partition,
		Metadata:  reason,
		CreatedAt: m.now(),
	})
	return t.Partition, nil
}

func (m *MemStore) GetTicketStats(_ context.Context, partition, userID string) (*TicketStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s TicketStats
	for _, t := range m.tickets {
		if !partitionMatch(partition, t.Partition) || (userID != "" && t.UserID != userID) {
			continue
		}
		s.Total++
		switch t.Status {
		case "closed", "resolved":
			s.Closed++
		case "waiting":
			s.Waiting++
		default:
			s.Open++
		}
	}
	return &s, nil
}

// ── Messages ──

func (m *MemStore) partitionOwnsTicket(partition, ticketID string) bool {
	t, ok := m.tickets[ticketID]
	return ok && partitionMatch(partition, t.Partition)
}

func (m *MemStore) AddMessage(_ context.Context, msg *TicketMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if msg.ID == "" {
		msg.ID = m.newID()
	}
	now := m.now()
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = now
	}
	msg.UpdatedAt = now
	m.messages = append(m.messages, cloneMessage(msg))
	return nil
}

func (m *MemStore) listMessages(partition, ticketID string, includeInternal bool, since *time.Time) []*TicketMessage {
	if !m.partitionOwnsTicket(partition, ticketID) {
		return nil
	}
	var out []*TicketMessage
	for _, msg := range m.messages {
		if msg.TicketID != ticketID {
			continue
		}
		if msg.IsInternal && !includeInternal {
			continue
		}
		if since != nil && !msg.CreatedAt.After(*since) {
			continue
		}
		out = append(out, cloneMessage(msg))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *MemStore) ListMessages(_ context.Context, partition, ticketID string, includeInternal bool) ([]*TicketMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listMessages(partition, ticketID, includeInternal, nil), nil
}

func (m *MemStore) ListMessagesSince(_ context.Context, partition, ticketID string, since time.Time, includeInternal bool) ([]*TicketMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listMessages(partition, ticketID, includeInternal, &since), nil
}

func (m *MemStore) SetMessageExternalRef(_ context.Context, messageID, refKey, refValue string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages {
		if msg.ID == messageID {
			if msg.ExternalRefs == nil {
				msg.ExternalRefs = map[string]string{}
			}
			msg.ExternalRefs[refKey] = refValue
			return nil
		}
	}
	return ErrNotFound
}

// ── Events ──

func (m *MemStore) AddTicketEvent(_ context.Context, e *TicketEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.ID == "" {
		e.ID = m.newID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = m.now()
	}
	cp := *e
	m.events = append(m.events, &cp)
	return nil
}

func (m *MemStore) ListTicketEvents(_ context.Context, partition, ticketID string) ([]*TicketEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.partitionOwnsTicket(partition, ticketID) {
		return nil, nil
	}
	var out []*TicketEvent
	for _, e := range m.events {
		if e.TicketID == ticketID {
			cp := *e
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// ── Attachments ──

func (m *MemStore) AddAttachment(_ context.Context, a *Attachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == "" {
		a.ID = m.newID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = m.now()
	}
	cp := *a
	m.attachments = append(m.attachments, &cp)
	return nil
}

func (m *MemStore) ListAttachments(_ context.Context, partition, ticketID string) ([]*Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.partitionOwnsTicket(partition, ticketID) {
		return nil, nil
	}
	var out []*Attachment
	for _, a := range m.attachments {
		if a.TicketID == ticketID {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

// ── clone helpers ──

func cloneTicket(t *Ticket) *Ticket {
	cp := *t
	cp.FormData = cloneAnyMap(t.FormData)
	cp.ExternalRefs = cloneStrMap(t.ExternalRefs)
	cp.ConfirmedAt = clonePtr(t.ConfirmedAt)
	cp.FirstResponseAt = clonePtr(t.FirstResponseAt)
	cp.ResolvedAt = clonePtr(t.ResolvedAt)
	cp.ClosedAt = clonePtr(t.ClosedAt)
	return &cp
}

func cloneMessage(m *TicketMessage) *TicketMessage {
	cp := *m
	cp.ExternalRefs = cloneStrMap(m.ExternalRefs)
	if m.Synced != nil {
		cp.Synced = make(map[string]bool, len(m.Synced))
		for k, v := range m.Synced {
			cp.Synced[k] = v
		}
	}
	return &cp
}

func clonePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}
