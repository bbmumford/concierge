package concierge

import (
	"context"
	"database/sql"
	"strings"
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

// ── Knowledge base ──

const categoryCols = `id, partition, name, slug, description, parent_id, sort_order, article_count,
	created_at, updated_at`

func scanCategory(sc interface{ Scan(...any) error }) (*KBCategory, error) {
	var c KBCategory
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Partition, &c.Name, &c.Slug, &c.Description, &c.ParentID, &c.SortOrder,
		&c.ArticleCount, &created, &updated); err != nil {
		return nil, err
	}
	c.CreatedAt = fromNanos(created)
	c.UpdatedAt = fromNanos(updated)
	return &c, nil
}

func (s *SQLStore) CreateCategory(ctx context.Context, c *KBCategory) error {
	if c.ID == "" {
		c.ID = randomID()
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	res, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_categories (`+categoryCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(partition, slug) DO NOTHING`,
		c.ID, c.Partition, c.Name, c.Slug, c.Description, c.ParentID, c.SortOrder, c.ArticleCount,
		tnanos(c.CreatedAt), tnanos(c.UpdatedAt))
	if err != nil {
		return err
	}
	return refuseUnwritten(res)
}

// refuseUnwritten turns an INSERT that ON CONFLICT ... DO NOTHING skipped into
// ErrDuplicateSlug, the answer MemStore gives for the same row, so a caller sees
// one refusal whichever store it holds rather than a driver's constraint text.
func refuseUnwritten(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDuplicateSlug
	}
	return nil
}

func (s *SQLStore) UpsertCategory(ctx context.Context, c *KBCategory) error {
	if c.ID == "" {
		c.ID = randomID()
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_categories (`+categoryCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(partition, slug) DO UPDATE SET name = excluded.name,
			description = excluded.description, parent_id = excluded.parent_id,
			sort_order = excluded.sort_order, article_count = excluded.article_count,
			updated_at = excluded.updated_at`,
		c.ID, c.Partition, c.Name, c.Slug, c.Description, c.ParentID, c.SortOrder, c.ArticleCount,
		tnanos(c.CreatedAt), tnanos(c.UpdatedAt)); err != nil {
		return err
	}
	// The read-back names the row by the exact key the statement above wrote, so
	// it matches the unique index rather than the guarded read predicate.
	var created int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, created_at FROM concierge_kb_categories
		WHERE partition = ? AND slug = ?`, c.Partition, c.Slug).Scan(&c.ID, &created); err != nil {
		return err
	}
	c.CreatedAt = fromNanos(created)
	return nil
}

func (s *SQLStore) GetCategoryBySlug(ctx context.Context, partition, slug string) (*KBCategory, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+categoryCols+` FROM concierge_kb_categories
		WHERE slug = ? AND (? <> '' AND partition = ?) LIMIT 1`, slug, partition, partition)
	c, err := scanCategory(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) ListCategories(ctx context.Context, partition string) ([]*KBCategory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+categoryCols+` FROM concierge_kb_categories
		WHERE (? <> '' AND partition = ?) ORDER BY sort_order ASC`, partition, partition)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KBCategory
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const articleCols = `id, partition, title, slug, content, content_html, category_id, category_name,
	category_slug, author_id, author_name, status, view_count, helpful_count, metadata,
	created_at, updated_at`

func scanArticle(sc interface{ Scan(...any) error }) (*KBArticle, error) {
	var a KBArticle
	var created, updated int64
	if err := sc.Scan(&a.ID, &a.Partition, &a.Title, &a.Slug, &a.Content, &a.ContentHTML, &a.CategoryID,
		&a.CategoryName, &a.CategorySlug, &a.AuthorID, &a.AuthorName, &a.Status, &a.ViewCount,
		&a.HelpfulCount, &a.Metadata, &created, &updated); err != nil {
		return nil, err
	}
	a.CreatedAt = fromNanos(created)
	a.UpdatedAt = fromNanos(updated)
	return &a, nil
}

func (s *SQLStore) CreateArticle(ctx context.Context, a *KBArticle) error {
	if a.ID == "" {
		a.ID = randomID()
	}
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	res, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_articles (`+articleCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(partition, category_slug, slug) DO NOTHING`,
		a.ID, a.Partition, a.Title, a.Slug, a.Content, a.ContentHTML, a.CategoryID, a.CategoryName,
		a.CategorySlug, a.AuthorID, a.AuthorName, a.Status, a.ViewCount, a.HelpfulCount, a.Metadata,
		tnanos(a.CreatedAt), tnanos(a.UpdatedAt))
	if err != nil {
		return err
	}
	return refuseUnwritten(res)
}

func (s *SQLStore) UpsertArticle(ctx context.Context, a *KBArticle) error {
	if a.ID == "" {
		a.ID = randomID()
	}
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if _, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_articles (`+articleCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(partition, category_slug, slug) DO UPDATE SET title = excluded.title,
			content = excluded.content, content_html = excluded.content_html,
			category_id = excluded.category_id, category_name = excluded.category_name,
			author_id = excluded.author_id, author_name = excluded.author_name,
			status = excluded.status, metadata = excluded.metadata, updated_at = excluded.updated_at`,
		a.ID, a.Partition, a.Title, a.Slug, a.Content, a.ContentHTML, a.CategoryID, a.CategoryName,
		a.CategorySlug, a.AuthorID, a.AuthorName, a.Status, a.ViewCount, a.HelpfulCount, a.Metadata,
		tnanos(a.CreatedAt), tnanos(a.UpdatedAt)); err != nil {
		return err
	}
	// The read-back names the row by the exact key the statement above wrote, as
	// UpsertCategory's does.
	var created int64
	if err := s.db.QueryRowContext(ctx, `SELECT id, created_at, view_count, helpful_count
		FROM concierge_kb_articles WHERE partition = ? AND category_slug = ? AND slug = ?`,
		a.Partition, a.CategorySlug, a.Slug).Scan(&a.ID, &created, &a.ViewCount, &a.HelpfulCount); err != nil {
		return err
	}
	a.CreatedAt = fromNanos(created)
	return nil
}

// GetArticleBySlug, ListArticles and SearchArticles each carry
// status = 'published' (ArticleStatusPublished): they are the public reads the
// Store contract describes. GetArticleBySlug orders by category_slug before
// LIMIT 1 because an empty categorySlug can match one article per category,
// and an unordered LIMIT 1 answers whichever row the engine reads first.
func (s *SQLStore) GetArticleBySlug(ctx context.Context, partition, categorySlug, articleSlug string) (*KBArticle, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+articleCols+` FROM concierge_kb_articles
		WHERE slug = ? AND (? <> '' AND partition = ?) AND (? = '' OR category_slug = ?)
			AND status = 'published' ORDER BY category_slug LIMIT 1`,
		articleSlug, partition, partition, categorySlug, categorySlug)
	a, err := scanArticle(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return a, err
}

func (s *SQLStore) queryArticles(ctx context.Context, where string, args ...any) ([]*KBArticle, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+articleCols+` FROM concierge_kb_articles
		WHERE `+where+` ORDER BY title ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*KBArticle
	for rows.Next() {
		a, err := scanArticle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLStore) ListArticles(ctx context.Context, partition, categorySlug string) ([]*KBArticle, error) {
	return s.queryArticles(ctx,
		`(? <> '' AND partition = ?) AND (? = '' OR category_slug = ?) AND status = 'published'`,
		partition, partition, categorySlug, categorySlug)
}

func (s *SQLStore) SearchArticles(ctx context.Context, partition, query string) ([]*KBArticle, error) {
	// Case-insensitive substring over title/content, partition-scoped. A LIKE with lower() keeps the
	// contract identical to the MemStore reference without a full-text index dependency.
	like := "%" + strings.ToLower(query) + "%"
	return s.queryArticles(ctx,
		`(? <> '' AND partition = ?) AND status = 'published' AND (lower(title) LIKE ? OR lower(content) LIKE ?)`,
		partition, partition, like, like)
}

func (s *SQLStore) IncrementArticleViewCount(ctx context.Context, articleID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE concierge_kb_articles SET view_count = view_count + 1
		WHERE id = ?`, articleID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
