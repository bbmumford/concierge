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
// durable (*sql.DB) store must match. Thread-safe; clock and id generator injectable. Owner is opaque;
// "" on an owner-scoped read means any owner.
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

func ownerMatch(want, have string) bool { return want == "" || want == have }

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

func (m *MemStore) GetTicket(_ context.Context, owner, id string) (*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok || !ownerMatch(owner, t.Owner) {
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

func (m *MemStore) ListTicketsByUser(_ context.Context, owner, userID, statusFilter string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return ownerMatch(owner, t.Owner) && t.UserID == userID && statusMatch(statusFilter, t.Status)
	}), nil
}

func (m *MemStore) ListTicketsByEmail(_ context.Context, owner, email string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return ownerMatch(owner, t.Owner) && strings.EqualFold(t.Email, email)
	}), nil
}

func (m *MemStore) ListTicketsByOrg(_ context.Context, owner, orgID, statusFilter string) ([]*Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.filterTickets(func(t *Ticket) bool {
		return ownerMatch(owner, t.Owner) && t.OrganisationID == orgID && statusMatch(statusFilter, t.Status)
	}), nil
}

func (m *MemStore) ListAllTickets(_ context.Context, statusFilter, categoryFilter string, limit, offset int) ([]*Ticket, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	all := m.filterTickets(func(t *Ticket) bool {
		return statusMatch(statusFilter, t.Status) && (categoryFilter == "" || t.Category == categoryFilter)
	})
	total := len(all)
	if offset >= total {
		return nil, total, nil
	}
	all = all[offset:]
	if limit > 0 && len(all) > limit {
		all = all[:limit]
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

func (m *MemStore) mutateTicket(owner, id string, fn func(*Ticket)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tickets[id]
	if !ok || !ownerMatch(owner, t.Owner) {
		return ErrNotFound
	}
	fn(t)
	t.UpdatedAt = m.now()
	return nil
}

func (m *MemStore) UpdateTicketStatus(_ context.Context, owner, id, status string) error {
	return m.mutateTicket(owner, id, func(t *Ticket) {
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

func (m *MemStore) UpdateTicketPriority(_ context.Context, owner, id, priority string) error {
	return m.mutateTicket(owner, id, func(t *Ticket) { t.Priority = priority })
}

func (m *MemStore) SetFirstResponseAt(_ context.Context, id string, at time.Time) error {
	return m.mutateTicket("", id, func(t *Ticket) {
		if t.FirstResponseAt == nil {
			a := at
			t.FirstResponseAt = &a
		}
	})
}

func (m *MemStore) SetExternalRef(_ context.Context, ticketID, refKey, refValue string) error {
	return m.mutateTicket("", ticketID, func(t *Ticket) {
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

func (m *MemStore) GetTicketStats(_ context.Context, owner, userID string) (*TicketStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s TicketStats
	for _, t := range m.tickets {
		if !ownerMatch(owner, t.Owner) || (userID != "" && t.UserID != userID) {
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

func (m *MemStore) ownerOwnsTicket(owner, ticketID string) bool {
	t, ok := m.tickets[ticketID]
	return ok && ownerMatch(owner, t.Owner)
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

func (m *MemStore) listMessages(owner, ticketID string, includeInternal bool, since *time.Time) []*TicketMessage {
	if owner != "" && !m.ownerOwnsTicket(owner, ticketID) {
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

func (m *MemStore) ListMessages(_ context.Context, owner, ticketID string, includeInternal bool) ([]*TicketMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listMessages(owner, ticketID, includeInternal, nil), nil
}

func (m *MemStore) ListMessagesSince(_ context.Context, owner, ticketID string, since time.Time, includeInternal bool) ([]*TicketMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listMessages(owner, ticketID, includeInternal, &since), nil
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

func (m *MemStore) ListTicketEvents(_ context.Context, owner, ticketID string) ([]*TicketEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner != "" && !m.ownerOwnsTicket(owner, ticketID) {
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

func (m *MemStore) ListAttachments(_ context.Context, owner, ticketID string) ([]*Attachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner != "" && !m.ownerOwnsTicket(owner, ticketID) {
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

// ── Knowledge base ──

func (m *MemStore) CreateCategory(_ context.Context, c *KBCategory) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		c.ID = m.newID()
	}
	now := m.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	m.categories[c.ID] = &cp
	return nil
}

func (m *MemStore) GetCategoryBySlug(_ context.Context, owner, slug string) (*KBCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.categories {
		if ownerMatch(owner, c.Owner) && c.Slug == slug {
			cp := *c
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListCategories(_ context.Context, owner string) ([]*KBCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*KBCategory
	for _, c := range m.categories {
		if ownerMatch(owner, c.Owner) {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

func (m *MemStore) CreateArticle(_ context.Context, a *KBArticle) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == "" {
		a.ID = m.newID()
	}
	now := m.now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	cp := *a
	m.articles[a.ID] = &cp
	return nil
}

func (m *MemStore) GetArticleBySlug(_ context.Context, owner, categorySlug, articleSlug string) (*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.articles {
		if ownerMatch(owner, a.Owner) && a.Slug == articleSlug && (categorySlug == "" || a.CategorySlug == categorySlug) {
			cp := *a
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListArticles(_ context.Context, owner, categorySlug string) ([]*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*KBArticle
	for _, a := range m.articles {
		if ownerMatch(owner, a.Owner) && (categorySlug == "" || a.CategorySlug == categorySlug) {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func (m *MemStore) SearchArticles(_ context.Context, owner, query string) ([]*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(query)
	var out []*KBArticle
	for _, a := range m.articles {
		if !ownerMatch(owner, a.Owner) {
			continue
		}
		if strings.Contains(strings.ToLower(a.Title), q) || strings.Contains(strings.ToLower(a.Content), q) {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func (m *MemStore) IncrementArticleViewCount(_ context.Context, articleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[articleID]
	if !ok {
		return ErrNotFound
	}
	a.ViewCount++
	return nil
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
