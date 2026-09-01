package concierge

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when the record does not exist.
var ErrNotFound = errors.New("concierge: not found")

// Store is the persistence contract for the help-desk: tickets, their message threads, the audit
// event log, attachments, and the knowledge base. A consumer supplies a *sql.DB store (the shipped
// default), a mesh-RPC gateway, or the reference MemStore. Every field is a neutral concierge type and
// the Partition is opaque — the store partitions on it, never interprets it. An empty partition ("") on an
// partition-scoped read means "any partition" (the admin/all-scope view). Times are time.Time.
type Store interface {
	// ── Tickets ──
	CreateTicket(ctx context.Context, t *Ticket) error
	GetTicket(ctx context.Context, partition, id string) (*Ticket, error)
	// GetTicketByExternalRef finds the ticket linked to an external channel thread (refKey chosen by
	// the adapter, e.g. "discord.channel"). Generalises the source's Discord-channel lookup.
	GetTicketByExternalRef(ctx context.Context, refKey, refValue string) (*Ticket, error)
	ListTicketsByUser(ctx context.Context, partition, userID, statusFilter string) ([]*Ticket, error)
	ListTicketsByEmail(ctx context.Context, partition, email string) ([]*Ticket, error)
	ListTicketsByOrg(ctx context.Context, partition, orgID, statusFilter string) ([]*Ticket, error)
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
