package concierge

import (
	"context"
	"sort"
	"time"
)

// memDesk is MemStore's desk half. Its zero value is empty and usable: every
// map is made on first write, under MemStore's lock, so NewMemStore needs no
// change for the desk and a MemStore built before the desk existed still works.
type memDesk struct {
	contacts    map[string]*Contact
	identities  []*ContactIdentity
	threadRefs  map[string]*ThreadRef
	teams       map[string]*Team
	queues      map[string]*Queue
	assignments map[string]*Assignment
	policies    map[string]*SLAPolicy
	slas        map[string]*TicketSLA
	ratings     map[string]*Rating
}

func (d *memDesk) init() {
	if d.contacts != nil {
		return
	}
	d.contacts = map[string]*Contact{}
	d.threadRefs = map[string]*ThreadRef{}
	d.teams = map[string]*Team{}
	d.queues = map[string]*Queue{}
	d.assignments = map[string]*Assignment{}
	d.policies = map[string]*SLAPolicy{}
	d.slas = map[string]*TicketSLA{}
	d.ratings = map[string]*Rating{}
}

func cloneContact(c *Contact) *Contact {
	cp := *c
	cp.Metadata = cloneStrMap(c.Metadata)
	return &cp
}

func cloneTicketSLA(s *TicketSLA) *TicketSLA {
	cp := *s
	cp.Pauses = make([]SLAPause, len(s.Pauses))
	for i, p := range s.Pauses {
		cp.Pauses[i] = SLAPause{From: p.From, To: clonePtr(p.To)}
	}
	return &cp
}

func clonePolicy(p *SLAPolicy) *SLAPolicy {
	cp := *p
	cp.PauseStatuses = append([]string(nil), p.PauseStatuses...)
	cp.Hours.Holidays = append([]string(nil), p.Hours.Holidays...)
	for d := range cp.Hours.Week {
		cp.Hours.Week[d] = append([]Window(nil), p.Hours.Week[d]...)
	}
	return &cp
}

// threadKey is the map key of a ThreadRef's unique pair.
func threadKey(channel, externalID string) string { return channel + "\x00" + externalID }

// ── Contacts ──

func (m *MemStore) CreateContact(_ context.Context, c *Contact) error {
	if c.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	id := c.ID
	if id == "" {
		id = m.newID()
	}
	if _, taken := m.desk.contacts[id]; taken {
		return ErrDuplicateID
	}
	now := m.now()
	row := cloneContact(c)
	row.ID, row.CreatedAt, row.UpdatedAt = id, now, now
	m.desk.contacts[id] = row
	c.ID, c.CreatedAt, c.UpdatedAt = id, now, now
	return nil
}

func (m *MemStore) contactIn(partition, id string) *Contact {
	c := m.desk.contacts[id]
	if c == nil || !partitionMatch(partition, c.Partition) {
		return nil
	}
	return c
}

func (m *MemStore) GetContact(_ context.Context, partition, id string) (*Contact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	c := m.contactIn(partition, id)
	if c == nil {
		return nil, ErrNotFound
	}
	return cloneContact(c), nil
}

func (m *MemStore) UpdateContact(_ context.Context, c *Contact) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	row := m.contactIn(c.Partition, c.ID)
	if row == nil {
		return ErrNotFound
	}
	row.Name, row.Email, row.AccountID, row.Metadata = c.Name, c.Email, c.AccountID, cloneStrMap(c.Metadata)
	row.UpdatedAt = m.now()
	c.CreatedAt, c.UpdatedAt = row.CreatedAt, row.UpdatedAt
	return nil
}

func (m *MemStore) LinkIdentity(_ context.Context, id *ContactIdentity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	if m.contactIn(id.Partition, id.ContactID) == nil {
		return ErrNotFound
	}
	for _, have := range m.desk.identities {
		if have.Partition != id.Partition || have.Channel != id.Channel || have.ExternalID != id.ExternalID {
			continue
		}
		if have.ContactID != id.ContactID {
			return ErrIdentityTaken
		}
		if id.VerifiedAt != nil {
			have.VerifiedAt = clonePtr(id.VerifiedAt)
		}
		id.CreatedAt = have.CreatedAt
		return nil
	}
	row := *id
	row.VerifiedAt = clonePtr(id.VerifiedAt)
	row.CreatedAt = m.now()
	m.desk.identities = append(m.desk.identities, &row)
	id.CreatedAt = row.CreatedAt
	return nil
}

func (m *MemStore) FindContactByIdentity(_ context.Context, partition, channel, externalID string) (*Contact, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	for _, have := range m.desk.identities {
		if partitionMatch(partition, have.Partition) && have.Channel == channel && have.ExternalID == externalID {
			if c := m.contactIn(partition, have.ContactID); c != nil {
				return cloneContact(c), nil
			}
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListContactIdentities(_ context.Context, partition, contactID string) ([]*ContactIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	var out []*ContactIdentity
	for _, have := range m.desk.identities {
		if partitionMatch(partition, have.Partition) && have.ContactID == contactID {
			cp := *have
			cp.VerifiedAt = clonePtr(have.VerifiedAt)
			out = append(out, &cp)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		return out[i].ExternalID < out[j].ExternalID
	})
	return out, nil
}

// ── Thread refs ──

func (m *MemStore) ClaimThreadRef(_ context.Context, r *ThreadRef) (bool, error) {
	if !validThreadRef(r) {
		return false, ErrInvalidThreadRef
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	key := threadKey(r.Channel, r.ExternalID)
	if have, ok := m.desk.threadRefs[key]; ok {
		*r = *have
		return false, nil
	}
	row := *r
	row.CreatedAt = m.now()
	m.desk.threadRefs[key] = &row
	r.CreatedAt = row.CreatedAt
	return true, nil
}

func (m *MemStore) GetThreadRef(_ context.Context, channel, externalID string) (*ThreadRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	have, ok := m.desk.threadRefs[threadKey(channel, externalID)]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *have
	return &cp, nil
}

// ── Teams, queues and assignment ──

func (m *MemStore) UpsertTeam(_ context.Context, t *Team) error {
	if t.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	now := m.now()
	for _, have := range m.desk.teams {
		if have.Partition == t.Partition && have.Slug == t.Slug {
			have.Name, have.Members, have.UpdatedAt = t.Name, append([]string(nil), t.Members...), now
			t.ID, t.CreatedAt, t.UpdatedAt = have.ID, have.CreatedAt, now
			return nil
		}
	}
	id := t.ID
	if id == "" {
		id = m.newID()
	}
	if _, taken := m.desk.teams[id]; taken {
		return ErrDuplicateID
	}
	row := *t
	row.ID, row.Members, row.CreatedAt, row.UpdatedAt = id, append([]string(nil), t.Members...), now, now
	m.desk.teams[id] = &row
	t.ID, t.CreatedAt, t.UpdatedAt = id, now, now
	return nil
}

func (m *MemStore) ListTeams(_ context.Context, partition string) ([]*Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	var out []*Team
	for _, have := range m.desk.teams {
		if partitionMatch(partition, have.Partition) {
			cp := *have
			cp.Members = append([]string(nil), have.Members...)
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (m *MemStore) UpsertQueue(_ context.Context, q *Queue) error {
	if q.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	now := m.now()
	for _, have := range m.desk.queues {
		if have.Partition == q.Partition && have.Slug == q.Slug {
			have.Name, have.TeamID, have.SLAPolicyID, have.UpdatedAt = q.Name, q.TeamID, q.SLAPolicyID, now
			q.ID, q.CreatedAt, q.UpdatedAt = have.ID, have.CreatedAt, now
			return nil
		}
	}
	id := q.ID
	if id == "" {
		id = m.newID()
	}
	if _, taken := m.desk.queues[id]; taken {
		return ErrDuplicateID
	}
	row := *q
	row.ID, row.CreatedAt, row.UpdatedAt = id, now, now
	m.desk.queues[id] = &row
	q.ID, q.CreatedAt, q.UpdatedAt = id, now, now
	return nil
}

func (m *MemStore) ListQueues(_ context.Context, partition string) ([]*Queue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	var out []*Queue
	for _, have := range m.desk.queues {
		if partitionMatch(partition, have.Partition) {
			cp := *have
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (m *MemStore) Assign(ctx context.Context, a *Assignment, actorType, actorID string) error {
	m.mu.Lock()
	m.desk.init()
	if !m.partitionOwnsTicket(a.Partition, a.TicketID) {
		m.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := m.desk.queues[a.QueueID]; a.QueueID != "" && !ok {
		m.mu.Unlock()
		return ErrUnknownRoute
	}
	if _, ok := m.desk.teams[a.TeamID]; a.TeamID != "" && !ok {
		m.mu.Unlock()
		return ErrUnknownRoute
	}
	old := assignmentValue(m.desk.assignments[a.TicketID])
	row := *a
	row.UpdatedAt, row.UpdatedBy = m.now(), actorID
	m.desk.assignments[a.TicketID] = &row
	a.UpdatedAt, a.UpdatedBy = row.UpdatedAt, row.UpdatedBy
	m.mu.Unlock()
	return m.AddTicketEvent(ctx, &TicketEvent{TicketID: a.TicketID, EventType: EventAssignment, ActorType: actorType,
		ActorID: actorID, OldValue: old, NewValue: assignmentValue(a)})
}

func (m *MemStore) GetAssignment(_ context.Context, partition, ticketID string) (*Assignment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	a := m.desk.assignments[ticketID]
	if a == nil || !partitionMatch(partition, a.Partition) {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

// discloseAcross writes EventCrossPartitionQueue on every ticket in page, before
// the page is returned, outside the lock AddTicketEvent takes itself.
func (m *MemStore) discloseAcross(ctx context.Context, page []*Ticket, actorType, actorID, reason string) error {
	for _, t := range page {
		if err := m.AddTicketEvent(ctx, &TicketEvent{TicketID: t.ID, EventType: EventCrossPartitionQueue,
			ActorType: actorType, ActorID: actorID, NewValue: t.Partition, Metadata: reason}); err != nil {
			return err
		}
	}
	return nil
}

// openStatus reports whether a ticket in status still waits for work.
func openStatus(status string) bool { return status != "closed" && status != "resolved" }

// oldestFirst orders tickets by creation, then id, the order both stores page in.
func oldestFirst(ts []*Ticket) {
	sort.SliceStable(ts, func(i, j int) bool {
		if !ts[i].CreatedAt.Equal(ts[j].CreatedAt) {
			return ts[i].CreatedAt.Before(ts[j].CreatedAt)
		}
		return ts[i].ID < ts[j].ID
	})
}

func (m *MemStore) ListQueueTickets(ctx context.Context, actorType, actorID, reason, queueID string, limit, offset int) ([]*Ticket, error) {
	if err := requireCrossPartitionTerms(actorType, actorID, reason, limit); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.desk.init()
	var all []*Ticket
	for id, a := range m.desk.assignments {
		if t := m.tickets[id]; t != nil && a.QueueID == queueID && openStatus(t.Status) {
			all = append(all, cloneTicket(t))
		}
	}
	m.mu.Unlock()
	oldestFirst(all)
	if offset < 0 {
		offset = 0
	}
	if offset >= len(all) {
		return nil, nil
	}
	all = all[offset:]
	if len(all) > limit {
		all = all[:limit]
	}
	if err := m.discloseAcross(ctx, all, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return all, nil
}

// ── Service levels ──

func (m *MemStore) UpsertSLAPolicy(_ context.Context, p *SLAPolicy) error {
	if p.Partition == "" {
		return ErrEmptyPartition
	}
	if !validSLAPolicy(p) {
		return ErrInvalidSLAPolicy
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	now := m.now()
	for _, have := range m.desk.policies {
		if have.Partition == p.Partition && have.Slug == p.Slug {
			row := clonePolicy(p)
			row.ID, row.CreatedAt, row.UpdatedAt = have.ID, have.CreatedAt, now
			m.desk.policies[have.ID] = row
			p.ID, p.CreatedAt, p.UpdatedAt = row.ID, row.CreatedAt, now
			return nil
		}
	}
	id := p.ID
	if id == "" {
		id = m.newID()
	}
	if _, taken := m.desk.policies[id]; taken {
		return ErrDuplicateID
	}
	row := clonePolicy(p)
	row.ID, row.CreatedAt, row.UpdatedAt = id, now, now
	m.desk.policies[id] = row
	p.ID, p.CreatedAt, p.UpdatedAt = id, now, now
	return nil
}

func (m *MemStore) GetSLAPolicy(_ context.Context, id string) (*SLAPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	p := m.desk.policies[id]
	if p == nil {
		return nil, ErrNotFound
	}
	return clonePolicy(p), nil
}

func (m *MemStore) ListSLAPolicies(_ context.Context, partition string) ([]*SLAPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	var out []*SLAPolicy
	for _, p := range m.desk.policies {
		if partitionMatch(partition, p.Partition) {
			out = append(out, clonePolicy(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (m *MemStore) StartSLA(_ context.Context, partition, ticketID, policyID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	if !m.partitionOwnsTicket(partition, ticketID) || m.desk.policies[policyID] == nil {
		return ErrNotFound
	}
	m.desk.slas[ticketID] = &TicketSLA{TicketID: ticketID, Partition: partition, PolicyID: policyID, StartedAt: at.UTC()}
	return nil
}

func (m *MemStore) runningSLA(partition, ticketID string) (*TicketSLA, error) {
	s := m.desk.slas[ticketID]
	if s == nil || !partitionMatch(partition, s.Partition) {
		return nil, ErrSLANotStarted
	}
	return s, nil
}

func (m *MemStore) PauseSLA(_ context.Context, partition, ticketID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	s, err := m.runningSLA(partition, ticketID)
	if err != nil {
		return err
	}
	if !s.Paused() {
		s.Pauses = append(s.Pauses, SLAPause{From: at.UTC()})
	}
	return nil
}

func (m *MemStore) ResumeSLA(_ context.Context, partition, ticketID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	s, err := m.runningSLA(partition, ticketID)
	if err != nil {
		return err
	}
	if s.Paused() {
		to := at.UTC()
		s.Pauses[len(s.Pauses)-1].To = &to
	}
	return nil
}

func (m *MemStore) GetTicketSLA(_ context.Context, partition, ticketID string) (*TicketSLA, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	s, err := m.runningSLA(partition, ticketID)
	if err != nil {
		return nil, err
	}
	return cloneTicketSLA(s), nil
}

func (m *MemStore) ListSLABreaches(ctx context.Context, actorType, actorID, reason string, now time.Time, limit int) ([]*Ticket, error) {
	if err := requireCrossPartitionTerms(actorType, actorID, reason, limit); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.desk.init()
	var all []*Ticket
	for id, s := range m.desk.slas {
		t, p := m.tickets[id], m.desk.policies[s.PolicyID]
		if t == nil || p == nil || !openStatus(t.Status) {
			continue
		}
		st, err := EvaluateSLA(p, s, t, now)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if st.FirstResponseBreached || st.ResolutionBreached {
			all = append(all, cloneTicket(t))
		}
	}
	m.mu.Unlock()
	oldestFirst(all)
	if len(all) > limit {
		all = all[:limit]
	}
	if err := m.discloseAcross(ctx, all, actorType, actorID, reason); err != nil {
		return nil, err
	}
	return all, nil
}

// ── Ratings ──

func (m *MemStore) RecordRating(_ context.Context, r *Rating) error {
	if !validRating(r) {
		return ErrInvalidRating
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	if !m.partitionOwnsTicket(r.Partition, r.TicketID) {
		return ErrNotFound
	}
	now := m.now()
	if have := m.desk.ratings[r.TicketID]; have != nil {
		have.Score, have.Comment, have.RatedBy, have.Source, have.UpdatedAt = r.Score, r.Comment, r.RatedBy, r.Source, now
		r.ID, r.CreatedAt, r.UpdatedAt = have.ID, have.CreatedAt, now
		return nil
	}
	row := *r
	if row.ID == "" {
		row.ID = m.newID()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	row.UpdatedAt = now
	m.desk.ratings[r.TicketID] = &row
	r.ID, r.CreatedAt, r.UpdatedAt = row.ID, row.CreatedAt, now
	return nil
}

func (m *MemStore) GetRating(_ context.Context, partition, ticketID string) (*Rating, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	r := m.desk.ratings[ticketID]
	if r == nil || !partitionMatch(partition, r.Partition) {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *MemStore) SummariseRatings(_ context.Context, partition string, since time.Time) (*RatingSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.desk.init()
	var sum RatingSummary
	total := 0
	for _, r := range m.desk.ratings {
		if partitionMatch(partition, r.Partition) && !r.CreatedAt.Before(since) {
			sum.Count++
			sum.Distribution[r.Score]++
			total += r.Score
		}
	}
	if sum.Count > 0 {
		sum.Average = float64(total) / float64(sum.Count)
	}
	return &sum, nil
}
