package concierge

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

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
