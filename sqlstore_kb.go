package concierge

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// ── Messages ──

// ownerOwnsTicket reports whether an owner-scoped caller may read a ticket's children. An empty owner
// is the admin view and always passes; otherwise the ticket must exist under that owner.
func (s *SQLStore) ownerOwnsTicket(ctx context.Context, owner, ticketID string) (bool, error) {
	if owner == "" {
		return true, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM concierge_tickets WHERE id = ? AND owner = ?`,
		ticketID, owner).Scan(&n)
	return n > 0, err
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

func (s *SQLStore) listMessages(ctx context.Context, owner, ticketID string, includeInternal bool, since *time.Time) ([]*TicketMessage, error) {
	ok, err := s.ownerOwnsTicket(ctx, owner, ticketID)
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

func (s *SQLStore) ListMessages(ctx context.Context, owner, ticketID string, includeInternal bool) ([]*TicketMessage, error) {
	return s.listMessages(ctx, owner, ticketID, includeInternal, nil)
}

func (s *SQLStore) ListMessagesSince(ctx context.Context, owner, ticketID string, since time.Time, includeInternal bool) ([]*TicketMessage, error) {
	return s.listMessages(ctx, owner, ticketID, includeInternal, &since)
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

func (s *SQLStore) ListTicketEvents(ctx context.Context, owner, ticketID string) ([]*TicketEvent, error) {
	ok, err := s.ownerOwnsTicket(ctx, owner, ticketID)
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

func (s *SQLStore) ListAttachments(ctx context.Context, owner, ticketID string) ([]*Attachment, error) {
	ok, err := s.ownerOwnsTicket(ctx, owner, ticketID)
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

const categoryCols = `id, owner, name, slug, description, parent_id, sort_order, article_count,
	created_at, updated_at`

func scanCategory(sc interface{ Scan(...any) error }) (*KBCategory, error) {
	var c KBCategory
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.Owner, &c.Name, &c.Slug, &c.Description, &c.ParentID, &c.SortOrder,
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_categories (`+categoryCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Owner, c.Name, c.Slug, c.Description, c.ParentID, c.SortOrder, c.ArticleCount,
		tnanos(c.CreatedAt), tnanos(c.UpdatedAt))
	return err
}

func (s *SQLStore) GetCategoryBySlug(ctx context.Context, owner, slug string) (*KBCategory, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+categoryCols+` FROM concierge_kb_categories
		WHERE slug = ? AND (? = '' OR owner = ?) LIMIT 1`, slug, owner, owner)
	c, err := scanCategory(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

func (s *SQLStore) ListCategories(ctx context.Context, owner string) ([]*KBCategory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+categoryCols+` FROM concierge_kb_categories
		WHERE (? = '' OR owner = ?) ORDER BY sort_order ASC`, owner, owner)
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

const articleCols = `id, owner, title, slug, content, content_html, category_id, category_name,
	category_slug, author_id, author_name, status, view_count, helpful_count, metadata,
	created_at, updated_at`

func scanArticle(sc interface{ Scan(...any) error }) (*KBArticle, error) {
	var a KBArticle
	var created, updated int64
	if err := sc.Scan(&a.ID, &a.Owner, &a.Title, &a.Slug, &a.Content, &a.ContentHTML, &a.CategoryID,
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO concierge_kb_articles (`+articleCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.Owner, a.Title, a.Slug, a.Content, a.ContentHTML, a.CategoryID, a.CategoryName,
		a.CategorySlug, a.AuthorID, a.AuthorName, a.Status, a.ViewCount, a.HelpfulCount, a.Metadata,
		tnanos(a.CreatedAt), tnanos(a.UpdatedAt))
	return err
}

func (s *SQLStore) GetArticleBySlug(ctx context.Context, owner, categorySlug, articleSlug string) (*KBArticle, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+articleCols+` FROM concierge_kb_articles
		WHERE slug = ? AND (? = '' OR owner = ?) AND (? = '' OR category_slug = ?) LIMIT 1`,
		articleSlug, owner, owner, categorySlug, categorySlug)
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

func (s *SQLStore) ListArticles(ctx context.Context, owner, categorySlug string) ([]*KBArticle, error) {
	return s.queryArticles(ctx, `(? = '' OR owner = ?) AND (? = '' OR category_slug = ?)`,
		owner, owner, categorySlug, categorySlug)
}

func (s *SQLStore) SearchArticles(ctx context.Context, owner, query string) ([]*KBArticle, error) {
	// Case-insensitive substring over title/content, owner-scoped. A LIKE with lower() keeps the
	// contract identical to the MemStore reference without a full-text index dependency.
	like := "%" + strings.ToLower(query) + "%"
	return s.queryArticles(ctx,
		`(? = '' OR owner = ?) AND (lower(title) LIKE ? OR lower(content) LIKE ?)`,
		owner, owner, like, like)
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
