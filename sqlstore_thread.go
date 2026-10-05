package concierge

import (
	"context"
	"database/sql"
	"time"
)

// ── Messages ──

// partitionOwnsTicket reports whether a caller may read a ticket's children: the
// ticket must exist under the partition the caller named. listMessages,
// ListTicketEvents and ListAttachments each gate on it and return the ticket's
// contents once it passes.
//
// An empty partition names no partition, so it owns nothing. A caller that does
// not know a ticket's partition asks ResolveTicketPartition for it, which records
// the request; reaching every partition by leaving a field unset would let any
// caller acquire that reach by forgetting to fill it in.
func (s *SQLStore) partitionOwnsTicket(ctx context.Context, partition, ticketID string) (bool, error) {
	if partition == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM concierge_tickets WHERE id = ? AND partition = ?`,
		ticketID, partition).Scan(&n)
	return n > 0, err
}

// ResolveTicketPartition returns the partition a ticket lives under, for a caller
// entitled to look across partitions, and writes a ticket event naming who asked
// and why before it answers. It is the only read here that is not confined to one
// partition; every other read takes the partition it returns and is scoped by it,
// so the reach is spent once and recorded once rather than riding along with a
// blank field.
//
// actorType, actorID and reason are all required: an unattributed privileged read
// is the condition this method exists to remove, so it refuses rather than
// answering anonymously. Returns ErrNotFound for a ticket that does not exist.
func (s *SQLStore) ResolveTicketPartition(ctx context.Context, actorType, actorID, ticketID, reason string) (string, error) {
	if actorType == "" || actorID == "" || reason == "" {
		return "", ErrActorRequired
	}
	var partition string
	err := s.db.QueryRowContext(ctx,
		`SELECT partition FROM concierge_tickets WHERE id = ?`, ticketID).Scan(&partition)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if err := s.AddTicketEvent(ctx, &TicketEvent{
		TicketID:  ticketID,
		EventType: EventCrossPartitionResolve,
		ActorType: actorType,
		ActorID:   actorID,
		NewValue:  partition,
		Metadata:  reason,
	}); err != nil {
		return "", err
	}
	return partition, nil
}

const messageCols = `id, ticket_id, author_type, author_id, author_name, author_email, content,
	content_html, external_refs, synced, is_internal, created_at, updated_at`

func scanMessage(sc interface{ Scan(...any) error }) (*TicketMessage, error) {
	var m TicketMessage
	var extRefs, synced string
	var internal int
	var created, updated int64
	if err := sc.Scan(&m.ID, &m.TicketID, &m.AuthorType, &m.AuthorID, &m.AuthorName, &m.AuthorEmail,
		&m.Content, &m.ContentHTML, &extRefs, &synced, &internal, &created, &updated); err != nil {
		return nil, err
	}
	m.ExternalRefs = readStrMap(extRefs)
	m.Synced = readBoolMap(synced)
	m.IsInternal = internal != 0
	m.CreatedAt = fromNanos(created)
	m.UpdatedAt = fromNanos(updated)
	return &m, nil
}

func (s *SQLStore) AddMessage(ctx context.Context, m *TicketMessage) error {
	if m.ID == "" {
		m.ID = randomID()
	}
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_messages (`+messageCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.TicketID, m.AuthorType, m.AuthorID, m.AuthorName, m.AuthorEmail, m.Content,
		m.ContentHTML, jsonStrMap(m.ExternalRefs), jsonBoolMap(m.Synced), boolInt(m.IsInternal),
		tnanos(m.CreatedAt), tnanos(m.UpdatedAt))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *SQLStore) listMessages(ctx context.Context, partition, ticketID string, includeInternal bool, since *time.Time) ([]*TicketMessage, error) {
	ok, err := s.partitionOwnsTicket(ctx, partition, ticketID)
	if err != nil || !ok {
		return nil, err
	}
	where := `ticket_id = ?`
	args := []any{ticketID}
	if !includeInternal {
		where += ` AND is_internal = 0`
	}
	if since != nil {
		where += ` AND created_at > ?`
		args = append(args, since.UnixNano())
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+messageCols+` FROM concierge_messages
		WHERE `+where+` ORDER BY created_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TicketMessage
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}

func (s *SQLStore) ListMessages(ctx context.Context, partition, ticketID string, includeInternal bool) ([]*TicketMessage, error) {
	return s.listMessages(ctx, partition, ticketID, includeInternal, nil)
}

func (s *SQLStore) ListMessagesSince(ctx context.Context, partition, ticketID string, since time.Time, includeInternal bool) ([]*TicketMessage, error) {
	return s.listMessages(ctx, partition, ticketID, includeInternal, &since)
}

func (s *SQLStore) SetMessageExternalRef(ctx context.Context, messageID, refKey, refValue string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE concierge_messages
		SET external_refs = json_set(external_refs, '$.' || ?, ?) WHERE id = ?`,
		refKey, refValue, messageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ── Events ──

func (s *SQLStore) AddTicketEvent(ctx context.Context, e *TicketEvent) error {
	if e.ID == "" {
		e.ID = randomID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_events
		(id, ticket_id, event_type, actor_type, actor_id, old_value, new_value, metadata, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		e.ID, e.TicketID, e.EventType, e.ActorType, e.ActorID, e.OldValue, e.NewValue, e.Metadata,
		tnanos(e.CreatedAt))
	return err
}

func (s *SQLStore) ListTicketEvents(ctx context.Context, partition, ticketID string) ([]*TicketEvent, error) {
	ok, err := s.partitionOwnsTicket(ctx, partition, ticketID)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, ticket_id, event_type, actor_type, actor_id,
		old_value, new_value, metadata, created_at FROM concierge_events
		WHERE ticket_id = ? ORDER BY created_at ASC`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TicketEvent
	for rows.Next() {
		var e TicketEvent
		var created int64
		if err := rows.Scan(&e.ID, &e.TicketID, &e.EventType, &e.ActorType, &e.ActorID, &e.OldValue,
			&e.NewValue, &e.Metadata, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = fromNanos(created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ── Attachments ──

func (s *SQLStore) AddAttachment(ctx context.Context, a *Attachment) error {
	if a.ID == "" {
		a.ID = randomID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_attachments
		(id, ticket_id, message_id, filename, content_type, size_bytes, storage_key, storage_url,
		uploaded_by, created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.TicketID, a.MessageID, a.Filename, a.ContentType, a.SizeBytes, a.StorageKey,
		a.StorageURL, a.UploadedBy, tnanos(a.CreatedAt))
	return err
}

func (s *SQLStore) ListAttachments(ctx context.Context, partition, ticketID string) ([]*Attachment, error) {
	ok, err := s.partitionOwnsTicket(ctx, partition, ticketID)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, ticket_id, message_id, filename, content_type,
		size_bytes, storage_key, storage_url, uploaded_by, created_at FROM concierge_attachments
		WHERE ticket_id = ? ORDER BY created_at ASC`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Attachment
	for rows.Next() {
		var a Attachment
		var created int64
		if err := rows.Scan(&a.ID, &a.TicketID, &a.MessageID, &a.Filename, &a.ContentType,
			&a.SizeBytes, &a.StorageKey, &a.StorageURL, &a.UploadedBy, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromNanos(created)
		out = append(out, &a)
	}
	return out, rows.Err()
}
