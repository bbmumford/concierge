package concierge

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when the record does not exist.
var ErrNotFound = errors.New("concierge: not found")

// ErrActorRequired is returned by ResolveTicketPartition when the caller supplies
// no actor type, no actor id, or no reason. That read crosses partitions, and the
// event it writes is the only record that it happened, so an anonymous one would
// produce reach with nothing to attribute it to.
var ErrActorRequired = errors.New("concierge: cross-partition read requires actor type, actor id and reason")

// EventCrossPartitionResolve is the ticket-event type ResolveTicketPartition
// writes before returning a partition. It is the audit trail for every read that
// was not confined to one partition; ListTicketEvents surfaces it to that
// ticket's own partition, so the partition sees who looked.
const EventCrossPartitionResolve = "cross_partition_resolve"

// Store is the persistence contract for the help-desk: tickets, their message threads, the audit
// event log, attachments, and the knowledge base. A consumer supplies a *sql.DB store (the shipped
// default), a mesh-RPC gateway, or the reference MemStore. Every field is a neutral concierge type and
// the Partition is opaque — the store partitions on it, never interprets it. Times are time.Time.
//
// An empty partition on a partition-scoped read matches nothing. A caller that
// needs to reach a ticket whose partition it does not know calls
// ResolveTicketPartition, which records who asked and why, and then reads with
// the partition that returns — so cross-partition reach is a thing a caller asks
// for and leaves a trace of, never a consequence of leaving a field blank.
type Store interface {
	// ── Tickets ──
	CreateTicket(ctx context.Context, t *Ticket) error
	GetTicket(ctx context.Context, partition, id string) (*Ticket, error)
	// GetTicketByExternalRef finds the ticket linked to an external channel thread (refKey chosen by
	// the adapter, e.g. "discord.channel"). Generalises the source's Discord-channel lookup.
	GetTicketByExternalRef(ctx context.Context, refKey, refValue string) (*Ticket, error)
	ListTicketsByUser(ctx context.Context, partition, userID, statusFilter string) ([]*Ticket, error)
	ListTicketsByEmail(ctx context.Context, partition, email string) ([]*Ticket, error)
	ListTicketsByGroup(ctx context.Context, partition, groupID, statusFilter string) ([]*Ticket, error)
	// ListAllTickets is the admin view across partitions, with optional status/category filters + paging;
	// returns the page and the total matching count.
	ListAllTickets(ctx context.Context, statusFilter, categoryFilter string, limit, offset int) (page []*Ticket, total int, err error)
	// ListBreachedTickets returns tickets past a first-response or resolution threshold (SLA breach).
	ListBreachedTickets(ctx context.Context, firstResponse, resolution time.Duration, now time.Time) ([]*Ticket, error)
	UpdateTicketStatus(ctx context.Context, partition, id, status string) error
	UpdateTicketPriority(ctx context.Context, partition, id, priority string) error
	SetFirstResponseAt(ctx context.Context, id string, at time.Time) error
	// SetExternalRef links (or clears, with an empty value) an external channel ref on a ticket.
	SetExternalRef(ctx context.Context, ticketID, refKey, refValue string) error
	GetTicketStats(ctx context.Context, partition, userID string) (*TicketStats, error)
	// ResolveTicketPartition names the partition a ticket lives under, for a caller
	// entitled to reach across partitions, and records the request as a ticket event
	// before answering. Every other read is confined to the partition it is given,
	// so this is where that confinement is lifted — deliberately, attributably, once.
	// All of actorType, actorID and reason are required.
	ResolveTicketPartition(ctx context.Context, actorType, actorID, ticketID, reason string) (string, error)

	// ── Messages ──
	AddMessage(ctx context.Context, m *TicketMessage) error
	ListMessages(ctx context.Context, partition, ticketID string, includeInternal bool) ([]*TicketMessage, error)
	ListMessagesSince(ctx context.Context, partition, ticketID string, since time.Time, includeInternal bool) ([]*TicketMessage, error)
	SetMessageExternalRef(ctx context.Context, messageID, refKey, refValue string) error

	// ── Events (append-only audit / recorded transitions) ──
	AddTicketEvent(ctx context.Context, e *TicketEvent) error
	ListTicketEvents(ctx context.Context, partition, ticketID string) ([]*TicketEvent, error)

	// ── Attachments (metadata; bytes live in an injected blob backend) ──
	AddAttachment(ctx context.Context, a *Attachment) error
	ListAttachments(ctx context.Context, partition, ticketID string) ([]*Attachment, error)

	// ── Knowledge base ──
	CreateCategory(ctx context.Context, c *KBCategory) error
	GetCategoryBySlug(ctx context.Context, partition, slug string) (*KBCategory, error)
	ListCategories(ctx context.Context, partition string) ([]*KBCategory, error)
	CreateArticle(ctx context.Context, a *KBArticle) error
	GetArticleBySlug(ctx context.Context, partition, categorySlug, articleSlug string) (*KBArticle, error)
	ListArticles(ctx context.Context, partition, categorySlug string) ([]*KBArticle, error)
	SearchArticles(ctx context.Context, partition, query string) ([]*KBArticle, error)
	IncrementArticleViewCount(ctx context.Context, articleID string) error
}
