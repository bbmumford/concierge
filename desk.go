package concierge

import (
	"context"
	"errors"
	"time"
)

// The desk half of the help desk: who a customer is across channels, the
// channel threads a ticket is reached through, the teams and queues tickets are
// routed to, service-level policies with business hours, and customer ratings.
// DeskStore is its persistence contract; MemStore and SQLStore both satisfy it,
// and a Store implementer that does not is still a complete ticket store,
// because the desk is an addition a consumer asserts for rather than a widening
// of Store.

// ErrIdentityTaken refuses LinkIdentity when another contact in the partition
// already holds the channel identity. One identity names one contact, so a
// channel adapter that resolves a sender by it reaches one person, never a
// choice between two.
var ErrIdentityTaken = errors.New("concierge: the channel identity belongs to another contact")

// ErrInvalidThreadRef refuses a ThreadRef that names no channel, no external id,
// no partition or no ticket. The ref is the key a resent delivery is recognised
// by, so a ref missing any of them could not be found again by the delivery
// that wrote it.
var ErrInvalidThreadRef = errors.New("concierge: a thread ref must name its channel, external id, partition and ticket")

// ErrInvalidRating refuses a rating whose score is outside MinRatingScore to
// MaxRatingScore, or that names no ticket.
var ErrInvalidRating = errors.New("concierge: a rating names its ticket and scores from 1 to 5")

// ErrInvalidSLAPolicy refuses a policy with no slug, a negative target, or
// business hours BusinessHours.Validate refuses.
var ErrInvalidSLAPolicy = errors.New("concierge: an SLA policy needs a slug, non-negative targets and valid business hours")

// ErrSLANotStarted is returned by PauseSLA, ResumeSLA and GetTicketSLA for a
// ticket no policy was started on.
var ErrSLANotStarted = errors.New("concierge: no SLA is running on the ticket")

// ErrUnknownRoute refuses an assignment naming a queue or team id no row holds,
// so a ticket is never routed to somewhere that cannot be listed.
var ErrUnknownRoute = errors.New("concierge: the assignment names a queue or team that does not exist")

// EventAssignment is the ticket-event type Assign writes: OldValue and NewValue
// are the previous and new assignment as "queue/team/assignee" ids, so the
// ticket's event log shows every routing change and who made it.
const EventAssignment = "assignment"

// EventCrossPartitionQueue is the ticket-event type ListQueueTickets and
// ListSLABreaches write on every ticket they return, the same disclosure record
// EventCrossPartitionList is for ListAllTickets.
const EventCrossPartitionQueue = "cross_partition_queue"

// The score range RecordRating accepts.
const (
	MinRatingScore = 1
	MaxRatingScore = 5
)

// Contact is one person a desk talks to, in one partition, however many
// channels they arrive through. AccountID is the opaque id of their account in
// the consuming platform when one is linked, and empty for a visitor nobody has
// identified.
type Contact struct {
	ID        string
	Partition string
	Name      string
	Email     string
	AccountID string
	Metadata  map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ContactIdentity is one channel's name for a contact: Channel is an opaque
// channel kind the adapter chooses ("email", "chat.visitor", "slack.user") and
// ExternalID the channel's own id for the person, namespaced by the adapter
// where the channel's ids are only unique inside a workspace or tenant.
// VerifiedAt is when the channel proved the person holds it (a confirmed code,
// a signed widget identity); nil means claimed, not proven.
type ContactIdentity struct {
	ContactID  string
	Partition  string
	Channel    string
	ExternalID string
	VerifiedAt *time.Time
	CreatedAt  time.Time
}

// ThreadRef ties one channel object (a conversation, a message) to the ticket
// and message it was recorded as. (Channel, ExternalID) is unique across the
// store: a channel's ids are its own, whichever partition the conversation
// belongs to, and ClaimThreadRef is how an adapter recognises a delivery it
// has already recorded. MessageID is empty for a conversation-level ref;
// ContactID names who the thread is with, when known.
type ThreadRef struct {
	Channel    string
	ExternalID string
	Partition  string
	TicketID   string
	MessageID  string
	ContactID  string
	CreatedAt  time.Time
}

// Team is a group of people a ticket can be routed to. Partition is the desk's
// partition: the one operating the desk, which need not be the partition of the
// tickets it routes. Members are opaque account ids.
type Team struct {
	ID        string
	Partition string
	Slug      string
	Name      string
	Members   []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Queue is where a ticket waits to be picked up, in the desk's partition. TeamID
// names the team that works it and SLAPolicyID the policy a ticket routed to it
// starts; either may be empty.
type Queue struct {
	ID          string
	Partition   string
	Slug        string
	Name        string
	TeamID      string
	SLAPolicyID string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Assignment is where a ticket is routed: its queue, team and assignee, any of
// which may be empty. Partition is the ticket's own.
type Assignment struct {
	TicketID   string
	Partition  string
	QueueID    string
	TeamID     string
	AssigneeID string
	UpdatedAt  time.Time
	UpdatedBy  string
}

// SLAPolicy is a desk's service-level targets: the time to a first response and
// to resolution, counted in the policy's business hours, with the clock stopped
// while the ticket is paused. PauseStatuses are the ticket statuses a consumer
// pauses the clock in (waiting on the customer, for one); the store records the
// pause when told, and does not watch statuses itself. A zero target sets no
// target for that leg.
type SLAPolicy struct {
	ID            string
	Partition     string
	Slug          string
	Name          string
	FirstResponse time.Duration
	Resolution    time.Duration
	Hours         BusinessHours
	PauseStatuses []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SLAPause is one interval the clock was stopped. To is nil while the pause is
// running.
type SLAPause struct {
	From time.Time
	To   *time.Time
}

// TicketSLA is the policy running on one ticket: when it started and every
// pause since. One ticket runs one policy; starting another replaces it.
type TicketSLA struct {
	TicketID  string
	Partition string
	PolicyID  string
	StartedAt time.Time
	Pauses    []SLAPause
}

// Paused reports whether the clock is stopped now.
func (s *TicketSLA) Paused() bool {
	return len(s.Pauses) > 0 && s.Pauses[len(s.Pauses)-1].To == nil
}

// Rating is a customer's satisfaction score for one ticket, MinRatingScore to
// MaxRatingScore. A ticket holds one: a later RecordRating replaces the score
// and comment and keeps the first rating's id and creation time, so a channel
// that delivers the same rating twice leaves one row.
type Rating struct {
	ID        string
	TicketID  string
	Partition string
	Score     int
	Comment   string
	RatedBy   string
	Source    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RatingSummary is the count, mean and per-score count of a partition's ratings.
// Distribution[n] counts ratings of score n; index 0 is unused.
type RatingSummary struct {
	Count        int
	Average      float64
	Distribution [MaxRatingScore + 1]int
}

// DeskStore is the persistence contract of the desk half. Every partition-scoped
// read matches nothing for an empty partition, as Store's do, and the two reads
// that cross partitions (ListQueueTickets, ListSLABreaches) carry Store's terms
// for ListAllTickets: actor type, actor id and reason are required, the limit
// must be positive, and an event is written on every ticket returned before the
// page is.
type DeskStore interface {
	// ── Contacts ──
	//
	// CreateContact refuses an empty partition with ErrEmptyPartition and a held
	// id with ErrDuplicateID, and mints an id when c names none.
	CreateContact(ctx context.Context, c *Contact) error
	GetContact(ctx context.Context, partition, id string) (*Contact, error)
	// UpdateContact replaces the name, email, account id and metadata of the
	// contact c names in c's partition; ErrNotFound when it holds none.
	UpdateContact(ctx context.Context, c *Contact) error
	// LinkIdentity gives a contact one more channel identity. Linking an
	// identity the contact already holds updates its VerifiedAt when the new one
	// is set and is otherwise a no-op; one another contact holds is
	// ErrIdentityTaken.
	LinkIdentity(ctx context.Context, id *ContactIdentity) error
	FindContactByIdentity(ctx context.Context, partition, channel, externalID string) (*Contact, error)
	ListContactIdentities(ctx context.Context, partition, contactID string) ([]*ContactIdentity, error)

	// ── Thread refs ──
	//
	// ClaimThreadRef records r when no ref holds (r.Channel, r.ExternalID) and
	// answers true. When one does, it answers false and overwrites r with the
	// stored ref, so the caller acts on the ticket the first delivery recorded.
	// Two concurrent claims of one key: exactly one answers true.
	ClaimThreadRef(ctx context.Context, r *ThreadRef) (bool, error)
	GetThreadRef(ctx context.Context, channel, externalID string) (*ThreadRef, error)

	// ── Teams, queues and assignment ──
	//
	// UpsertTeam and UpsertQueue write under (Partition, Slug) the way
	// UpsertCategory does: insert, or replace the stored row's content keeping
	// its id and creation time, and set the struct's ID and CreatedAt to the
	// stored row's.
	UpsertTeam(ctx context.Context, t *Team) error
	ListTeams(ctx context.Context, partition string) ([]*Team, error)
	UpsertQueue(ctx context.Context, q *Queue) error
	ListQueues(ctx context.Context, partition string) ([]*Queue, error)
	// Assign routes the ticket a names, which must exist under a.Partition, to
	// a's queue, team and assignee, and writes an EventAssignment by the actor
	// naming the old and new routes. A queue or team id no row holds is
	// ErrUnknownRoute.
	Assign(ctx context.Context, a *Assignment, actorType, actorID string) error
	GetAssignment(ctx context.Context, partition, ticketID string) (*Assignment, error)
	// ListQueueTickets is a queue's work across partitions, oldest first, with
	// the closed and resolved tickets left out.
	ListQueueTickets(ctx context.Context, actorType, actorID, reason, queueID string, limit, offset int) ([]*Ticket, error)

	// ── Service levels ──
	UpsertSLAPolicy(ctx context.Context, p *SLAPolicy) error
	GetSLAPolicy(ctx context.Context, id string) (*SLAPolicy, error)
	ListSLAPolicies(ctx context.Context, partition string) ([]*SLAPolicy, error)
	// StartSLA runs policyID on the ticket from at, replacing any policy it ran.
	StartSLA(ctx context.Context, partition, ticketID, policyID string, at time.Time) error
	// PauseSLA stops the ticket's clock at at; a clock already stopped stays
	// stopped from its first pause. ResumeSLA restarts it; a running clock is
	// left running.
	PauseSLA(ctx context.Context, partition, ticketID string, at time.Time) error
	ResumeSLA(ctx context.Context, partition, ticketID string, at time.Time) error
	GetTicketSLA(ctx context.Context, partition, ticketID string) (*TicketSLA, error)
	// ListSLABreaches is every ticket across partitions whose running policy's
	// first-response or resolution target EvaluateSLA reports breached at now.
	ListSLABreaches(ctx context.Context, actorType, actorID, reason string, now time.Time, limit int) ([]*Ticket, error)

	// ── Ratings ──
	RecordRating(ctx context.Context, r *Rating) error
	GetRating(ctx context.Context, partition, ticketID string) (*Rating, error)
	SummariseRatings(ctx context.Context, partition string, since time.Time) (*RatingSummary, error)
}

var (
	_ DeskStore = (*MemStore)(nil)
	_ DeskStore = (*SQLStore)(nil)
)

// SLAStatus is EvaluateSLA's answer for one ticket at one instant. Each elapsed
// figure is business time with the pauses taken out, up to the leg's stamp
// (FirstResponseAt, ResolvedAt) when it is set, else to now.
type SLAStatus struct {
	FirstResponseElapsed  time.Duration
	ResolutionElapsed     time.Duration
	FirstResponseBreached bool
	ResolutionBreached    bool
	Paused                bool
}

// EvaluateSLA measures the ticket's running policy at now. A leg whose target is
// zero is never breached; a leg already stamped is breached only when its stamp
// came after the target.
func EvaluateSLA(p *SLAPolicy, s *TicketSLA, t *Ticket, now time.Time) (SLAStatus, error) {
	if p == nil || s == nil || t == nil {
		return SLAStatus{}, ErrSLANotStarted
	}
	measure := func(stamp *time.Time) (time.Duration, error) {
		end := now
		if stamp != nil {
			end = *stamp
		}
		return slaElapsed(p.Hours, s, end)
	}
	first, err := measure(t.FirstResponseAt)
	if err != nil {
		return SLAStatus{}, err
	}
	resolution, err := measure(t.ResolvedAt)
	if err != nil {
		return SLAStatus{}, err
	}
	return SLAStatus{
		FirstResponseElapsed:  first,
		ResolutionElapsed:     resolution,
		FirstResponseBreached: p.FirstResponse > 0 && first > p.FirstResponse,
		ResolutionBreached:    p.Resolution > 0 && resolution > p.Resolution,
		Paused:                s.Paused(),
	}, nil
}

// slaElapsed is the business time from s.StartedAt to end, less the business
// time of every pause inside it.
func slaElapsed(h BusinessHours, s *TicketSLA, end time.Time) (time.Duration, error) {
	if !end.After(s.StartedAt) {
		return 0, nil
	}
	total, err := h.Elapsed(s.StartedAt, end)
	if err != nil {
		return 0, err
	}
	for _, p := range s.Pauses {
		to := end
		if p.To != nil && p.To.Before(end) {
			to = *p.To
		}
		from := p.From
		if from.Before(s.StartedAt) {
			from = s.StartedAt
		}
		if !to.After(from) {
			continue
		}
		paused, err := h.Elapsed(from, to)
		if err != nil {
			return 0, err
		}
		total -= paused
	}
	if total < 0 {
		total = 0
	}
	return total, nil
}

// assignmentValue is an assignment's form in EventAssignment's old and new values.
func assignmentValue(a *Assignment) string {
	if a == nil {
		return "//"
	}
	return a.QueueID + "/" + a.TeamID + "/" + a.AssigneeID
}

// validRating reports whether RecordRating may store r.
func validRating(r *Rating) bool {
	return r.TicketID != "" && r.Score >= MinRatingScore && r.Score <= MaxRatingScore
}

// validThreadRef reports whether ClaimThreadRef may store r.
func validThreadRef(r *ThreadRef) bool {
	return r.Channel != "" && r.ExternalID != "" && r.Partition != "" && r.TicketID != ""
}

// validSLAPolicy reports whether UpsertSLAPolicy may store p.
func validSLAPolicy(p *SLAPolicy) bool {
	return p.Slug != "" && p.FirstResponse >= 0 && p.Resolution >= 0 && p.Hours.Validate() == nil
}

// requireCrossPartitionTerms is the precondition both desk reads across
// partitions share with ListAllTickets.
func requireCrossPartitionTerms(actorType, actorID, reason string, limit int) error {
	if actorType == "" || actorID == "" || reason == "" {
		return ErrActorRequired
	}
	if limit <= 0 {
		return ErrUnboundedAdminList
	}
	return nil
}
