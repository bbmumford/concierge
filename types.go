// Package concierge is the scope-agnostic help-desk / support system (extracted from the HSTLES
// support domain). It owns tickets, their message threads, an audit event log, attachments, and a
// knowledge base — over an injected store, with pluggable delivery/thread channels and an opaque
// partition-scope the system never interprets. The primary axis (was a baked tenant) is an opaque Partition;
// org/app are opaque secondary fields; the Discord-specific thread linkage is generalised to
// channel-agnostic ExternalRefs so any channel adapter (Discord, email, Slack) can link a ticket to
// its external thread and look the ticket up by it. Times are time.Time; nil *time.Time means "not
// yet" (unconfirmed / unresolved / open).
package concierge

import "time"

// Ticket is a support request. Partition is the opaque partition-scope (the store partitions on it, never
// interprets it); GroupID/AppID are opaque secondary grouping fields a consumer may filter by.
type Ticket struct {
	ID       string
	Number   string
	Partition    string // opaque partition-scope (was tenant)
	UserID   string
	Email    string
	Subject  string
	Category string
	Priority string
	Status   string
	FormData map[string]any

	GroupID string // opaque secondary field
	AppID          string // opaque secondary field

	// ExternalRefs links the ticket to external channel threads, keyed by an opaque ref name a channel
	// adapter chooses (e.g. "discord.channel", "discord.message", "slack.thread"). Generalises the
	// source's baked Discord IDs so the store lookup and the adapters stay channel-agnostic.
	ExternalRefs map[string]string

	Source    string
	UserAgent string
	IPAddress string

	CreatedAt       time.Time
	UpdatedAt       time.Time
	ConfirmedAt     *time.Time
	FirstResponseAt *time.Time
	ResolvedAt      *time.Time
	ClosedAt        *time.Time
}

// TicketMessage is one message in a ticket conversation. AuthorType distinguishes user/agent/system;
// IsInternal marks an agent-only note.
type TicketMessage struct {
	ID          string
	TicketID    string
	AuthorType  string
	AuthorID    string
	AuthorName  string
	AuthorEmail string
	Content     string
	ContentHTML string
	// ExternalRefs links the message to external channel messages (e.g. "discord.message"), keyed like
	// Ticket.ExternalRefs; Synced records which channels the message has been mirrored to.
	ExternalRefs map[string]string
	Synced       map[string]bool
	IsInternal   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TicketEvent is an append-only audit entry — a recorded transition (status/priority/assignment
// change), never an inferred mutation. Old/New capture the value change; Metadata is opaque JSON.
type TicketEvent struct {
	ID        string
	TicketID  string
	EventType string
	ActorType string
	ActorID   string
	OldValue  string
	NewValue  string
	Metadata  string
	CreatedAt time.Time
}

// Attachment is a file on a ticket or message. StorageKey/StorageURL point at the injected blob
// backend; the store holds only metadata.
type Attachment struct {
	ID          string
	TicketID    string
	MessageID   string
	Filename    string
	ContentType string
	SizeBytes   int64
	StorageKey  string
	StorageURL  string
	UploadedBy  string
	CreatedAt   time.Time
}

// KBCategory is a knowledge-base category (a tree via ParentID). Partition is the opaque partition-scope.
type KBCategory struct {
	ID           string
	Partition        string
	Name         string
	Slug         string
	Description  string
	ParentID     string
	SortOrder    int
	ArticleCount int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// KBArticle is a knowledge-base article.
type KBArticle struct {
	ID           string
	Partition        string
	Title        string
	Slug         string
	Content      string
	ContentHTML  string
	CategoryID   string
	CategoryName string
	CategorySlug string
	AuthorID     string
	AuthorName   string
	Status       string
	ViewCount    int
	HelpfulCount int
	Metadata     string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// TicketStats holds per-scope ticket counts.
type TicketStats struct {
	Total   int
	Open    int
	Waiting int
	Closed  int
}
