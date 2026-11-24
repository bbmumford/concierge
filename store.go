package concierge

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by single-record reads when the record does not exist.
var ErrNotFound = errors.New("concierge: not found")

// ErrInvalidRefKey refuses an ExternalRefs key that is empty or holds a double
// quote. A key is any other opaque name, dotted ones such as "discord.channel"
// included; the SQL store addresses it as one quoted member of the JSON object,
// and a quote would end that member's name early.
var ErrInvalidRefKey = errors.New("concierge: an external ref key must be non-empty and hold no double quote")

// ErrEmptyPartition refuses a write that would store a ticket, category or
// article under no partition. Every read confined to a partition answers
// nothing for the empty one, so such a row could never be read back by its
// partition; it is refused instead of stored unreachable.
var ErrEmptyPartition = errors.New("concierge: a ticket, category or article must name its partition")

// ErrActorRequired is returned by the cross-partition reads — ResolveTicketPartition
// and ListAllTickets — when the caller supplies no actor type, no actor id, or no
// reason. Those reads cross partitions, and the events they write are the only
// record that they happened, so an anonymous one would produce reach with nothing
// to attribute it to.
var ErrActorRequired = errors.New("concierge: cross-partition read requires actor type, actor id and reason")

// EventCrossPartitionResolve is the ticket-event type ResolveTicketPartition
// writes before returning a partition. It is the audit trail for every read that
// was not confined to one partition; ListTicketEvents surfaces it to that
// ticket's own partition, so the partition sees who looked.
const EventCrossPartitionResolve = "cross_partition_resolve"

// EventCrossPartitionList is the ticket-event type ListAllTickets writes, one per
// ticket in the page it returns, before returning it. Keying it to each disclosed
// ticket rather than to the call means ListTicketEvents shows a partition every
// cross-partition read that actually reached one of its tickets — a partition can
// see that it was read without being able to see the rest of the page.
const EventCrossPartitionList = "cross_partition_list"

// ErrUnboundedAdminList is returned by ListAllTickets for a non-positive limit.
// The audit is one event per disclosed ticket, so an unbounded page is an
// unbounded write; requiring a limit keeps the record proportional to the read and
// makes the caller name how much it is taking.
var ErrUnboundedAdminList = errors.New("concierge: cross-partition list requires a positive limit")

// ErrDuplicateSlug is returned by CreateCategory when the partition already holds
// a category with that slug, and by CreateArticle when the partition already holds
// an article with that slug in that category. Both stores answer it, so a caller
// written against MemStore sees the same refusal from SQLStore, where the unique
// indexes InitSchema builds are what refuse the row. A caller that means "insert
// or replace" calls UpsertCategory or UpsertArticle instead.
var ErrDuplicateSlug = errors.New("concierge: slug already taken")

// ErrDuplicateID is returned by CreateTicket, CreateCategory and CreateArticle,
// and by UpsertCategory and UpsertArticle where they insert, when the caller names
// an id that a row under another key already holds; for a ticket the id is the
// only key, so any ticket holding it refuses the create, whatever its partition. SQLStore's primary key refuses
// such a row and MemStore refuses it the same way, so neither store replaces a
// row the caller did not address by its key. A key that is taken as well is
// answered first, as ErrDuplicateSlug from a create and as a replace from an
// upsert, on both stores. A refused write leaves the caller's struct as given.
var ErrDuplicateID = errors.New("concierge: id already taken")

// ticketNumberLen is how many leading characters of a ticket's id make its
// default number, the short reference a person quotes back to support.
const ticketNumberLen = 8

// ticketNumber is CreateTicket's default number for id: its first ticketNumberLen
// characters, or the whole id when it is shorter, cut on a character boundary so
// a caller-supplied id of any length or alphabet yields a valid string. Both
// stores call it, so a ticket created through either carries the same number.
func ticketNumber(id string) string {
	n := 0
	for i := range id {
		if n == ticketNumberLen {
			return id[:i]
		}
		n++
	}
	return id
}

// ArticleStatusPublished is the KBArticle.Status value the public reads serve.
// ListArticles, SearchArticles and GetArticleBySlug answer only articles carrying
// it; an article under any other status (a draft, an archived article) is stored
// and absent from all three.
const ArticleStatusPublished = "published"

// Store is the persistence contract for the help-desk: tickets, their message threads, the audit
// event log, attachments, and the knowledge base. A consumer supplies a *sql.DB store (the shipped
// default), a mesh-RPC gateway, or the reference MemStore. Every field is a neutral concierge type and
// the Partition is opaque — the store partitions on it, never interprets it. Times are time.Time.
//
// An empty partition on a partition-scoped read matches nothing. A caller that
// needs to reach a ticket whose partition it does not know calls
// ResolveTicketPartition, and one that needs the view across partitions calls
// ListAllTickets; both record who asked and why — so cross-partition reach is a
// thing a caller asks for and leaves a trace of, never a consequence of leaving a
// field blank.
type Store interface {
	// ── Tickets ──
	//
	// CreateTicket, CreateCategory, UpsertCategory, CreateArticle and
	// UpsertArticle refuse a row with an empty Partition with ErrEmptyPartition.
	CreateTicket(ctx context.Context, t *Ticket) error
	GetTicket(ctx context.Context, partition, id string) (*Ticket, error)
	// GetTicketByExternalRef finds the ticket linked to an external channel thread (refKey chosen by
	// the adapter, e.g. "discord.channel"). Generalises the source's Discord-channel lookup.
	GetTicketByExternalRef(ctx context.Context, refKey, refValue string) (*Ticket, error)
	ListTicketsByUser(ctx context.Context, partition, userID, statusFilter string) ([]*Ticket, error)
	ListTicketsByEmail(ctx context.Context, partition, email string) ([]*Ticket, error)
	ListTicketsByGroup(ctx context.Context, partition, groupID, statusFilter string) ([]*Ticket, error)
	// ListAllTickets is the admin view across partitions, with optional status/category
	// filters + paging; returns the page and the total matching count. It is the second
	// read here that is not confined to one partition, and it carries the same terms as
	// ResolveTicketPartition: actorType, actorID and reason are all required, and it
	// writes an EventCrossPartitionList on every ticket it returns before returning it.
	// limit must be positive — see ErrUnboundedAdminList.
	ListAllTickets(ctx context.Context, actorType, actorID, reason, statusFilter, categoryFilter string, limit, offset int) (page []*Ticket, total int, err error)
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
	//
	// A category's slug is unique within its partition, and an article's slug is
	// unique within its partition and category. CreateCategory and CreateArticle
	// refuse a taken slug with ErrDuplicateSlug.
	CreateCategory(ctx context.Context, c *KBCategory) error
	// UpsertCategory writes c under its (Partition, Slug): it inserts the category
	// when the partition holds no such slug and otherwise replaces the stored row's
	// name, description, parent, sort order and article count, keeping its id and
	// creation time. Either way c.ID and c.CreatedAt are set to the stored row's,
	// so repeating the call leaves one row carrying the latest content: a writer
	// that repeats the same writes at every start, over a database that may
	// already hold their rows, leaves one row per key.
	UpsertCategory(ctx context.Context, c *KBCategory) error
	GetCategoryBySlug(ctx context.Context, partition, slug string) (*KBCategory, error)
	ListCategories(ctx context.Context, partition string) ([]*KBCategory, error)
	CreateArticle(ctx context.Context, a *KBArticle) error
	// UpsertArticle writes a under its (Partition, CategorySlug, Slug), the same
	// way UpsertCategory writes a category. It replaces the authored fields (title,
	// content, category, author, status, metadata) and keeps the stored id,
	// creation time, view count and helpful count. The two counts are not
	// authored content: IncrementArticleViewCount adds to the view count between
	// writes, and the helpful count is written only when the row is inserted, so
	// a repeated upsert resets neither. a.ID, a.CreatedAt, a.ViewCount and
	// a.HelpfulCount are set to the stored row's.
	UpsertArticle(ctx context.Context, a *KBArticle) error
	// GetArticleBySlug, ListArticles and SearchArticles are the public reads: a
	// help centre serves them to visitors who present no principal, so each
	// answers only articles whose Status is ArticleStatusPublished. A draft's slug
	// is a miss (ErrNotFound) and a draft is absent from every listing and search
	// result.
	//
	// GetArticleBySlug with an empty categorySlug matches the slug in any
	// category. The key holds one article per category under a slug, so where
	// several categories publish it, the one whose category slug sorts first
	// answers, on every call and on both stores.
	GetArticleBySlug(ctx context.Context, partition, categorySlug, articleSlug string) (*KBArticle, error)
	ListArticles(ctx context.Context, partition, categorySlug string) ([]*KBArticle, error)
	SearchArticles(ctx context.Context, partition, query string) ([]*KBArticle, error)
	IncrementArticleViewCount(ctx context.Context, articleID string) error
}
