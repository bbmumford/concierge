package concierge

import (
	"context"
	"sort"
	"strings"
)

// ── Knowledge base ──

// categoryAt returns the stored category under exactly (partition, slug), the
// key SQLStore's unique index on concierge_kb_categories holds, or nil. It
// compares the partition by equality rather than through partitionMatch because
// it names a write key, which an empty partition is as much as any other value.
// Callers hold m.mu.
func (m *MemStore) categoryAt(partition, slug string) *KBCategory {
	for _, c := range m.categories {
		if c.Partition == partition && c.Slug == slug {
			return c
		}
	}
	return nil
}

// articleAt is categoryAt for articles, keyed (partition, categorySlug, slug) as
// SQLStore's unique index on concierge_kb_articles is. Callers hold m.mu.
func (m *MemStore) articleAt(partition, categorySlug, slug string) *KBArticle {
	for _, a := range m.articles {
		if a.Partition == partition && a.CategorySlug == categorySlug && a.Slug == slug {
			return a
		}
	}
	return nil
}

func (m *MemStore) CreateCategory(_ context.Context, c *KBCategory) error {
	if c.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.categoryAt(c.Partition, c.Slug) != nil {
		return ErrDuplicateSlug
	}
	if _, held := m.categories[c.ID]; c.ID != "" && held {
		return ErrDuplicateID
	}
	if c.ID == "" {
		c.ID = m.newID()
	}
	now := m.now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	m.categories[c.ID] = &cp
	return nil
}

func (m *MemStore) UpsertCategory(_ context.Context, c *KBCategory) error {
	if c.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if stored := m.categoryAt(c.Partition, c.Slug); stored != nil {
		stored.Name = c.Name
		stored.Description = c.Description
		stored.ParentID = c.ParentID
		stored.SortOrder = c.SortOrder
		stored.ArticleCount = c.ArticleCount
		stored.UpdatedAt = now
		c.ID = stored.ID
		c.CreatedAt = stored.CreatedAt
		c.UpdatedAt = now
		return nil
	}
	if _, held := m.categories[c.ID]; c.ID != "" && held {
		return ErrDuplicateID
	}
	c.UpdatedAt = now
	if c.ID == "" {
		c.ID = m.newID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	cp := *c
	m.categories[c.ID] = &cp
	return nil
}

func (m *MemStore) GetCategoryBySlug(_ context.Context, partition, slug string) (*KBCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.categories {
		if partitionMatch(partition, c.Partition) && c.Slug == slug {
			cp := *c
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) ListCategories(_ context.Context, partition string) ([]*KBCategory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*KBCategory
	for _, c := range m.categories {
		if partitionMatch(partition, c.Partition) {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out, nil
}

func (m *MemStore) CreateArticle(_ context.Context, a *KBArticle) error {
	if a.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.articleAt(a.Partition, a.CategorySlug, a.Slug) != nil {
		return ErrDuplicateSlug
	}
	if _, held := m.articles[a.ID]; a.ID != "" && held {
		return ErrDuplicateID
	}
	if a.ID == "" {
		a.ID = m.newID()
	}
	now := m.now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	cp := *a
	m.articles[a.ID] = &cp
	return nil
}

func (m *MemStore) UpsertArticle(_ context.Context, a *KBArticle) error {
	if a.Partition == "" {
		return ErrEmptyPartition
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if stored := m.articleAt(a.Partition, a.CategorySlug, a.Slug); stored != nil {
		stored.Title = a.Title
		stored.Content = a.Content
		stored.ContentHTML = a.ContentHTML
		stored.CategoryID = a.CategoryID
		stored.CategoryName = a.CategoryName
		stored.AuthorID = a.AuthorID
		stored.AuthorName = a.AuthorName
		stored.Status = a.Status
		stored.Metadata = a.Metadata
		stored.UpdatedAt = now
		a.ID = stored.ID
		a.CreatedAt = stored.CreatedAt
		a.UpdatedAt = now
		a.ViewCount = stored.ViewCount
		a.HelpfulCount = stored.HelpfulCount
		return nil
	}
	if _, held := m.articles[a.ID]; a.ID != "" && held {
		return ErrDuplicateID
	}
	a.UpdatedAt = now
	if a.ID == "" {
		a.ID = m.newID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	cp := *a
	m.articles[a.ID] = &cp
	return nil
}

// publicArticle reports whether a public read may answer a: it lives in the
// partition the caller named and carries ArticleStatusPublished. GetArticleBySlug,
// ListArticles and SearchArticles filter through it, matching the
// status = 'published' term in SQLStore's three queries.
func publicArticle(partition string, a *KBArticle) bool {
	return partitionMatch(partition, a.Partition) && a.Status == ArticleStatusPublished
}

func (m *MemStore) GetArticleBySlug(_ context.Context, partition, categorySlug, articleSlug string) (*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The first category slug answers, as SQLStore's ORDER BY category_slug
	// does; ranging over the map alone would answer a different category on
	// different calls.
	var found *KBArticle
	for _, a := range m.articles {
		if publicArticle(partition, a) && a.Slug == articleSlug && (categorySlug == "" || a.CategorySlug == categorySlug) {
			if found == nil || a.CategorySlug < found.CategorySlug {
				found = a
			}
		}
	}
	if found == nil {
		return nil, ErrNotFound
	}
	cp := *found
	return &cp, nil
}

func (m *MemStore) ListArticles(_ context.Context, partition, categorySlug string) ([]*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*KBArticle
	for _, a := range m.articles {
		if publicArticle(partition, a) && (categorySlug == "" || a.CategorySlug == categorySlug) {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func (m *MemStore) SearchArticles(_ context.Context, partition, query string) ([]*KBArticle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := strings.ToLower(query)
	var out []*KBArticle
	for _, a := range m.articles {
		if !publicArticle(partition, a) {
			continue
		}
		if strings.Contains(strings.ToLower(a.Title), q) || strings.Contains(strings.ToLower(a.Content), q) {
			cp := *a
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func (m *MemStore) IncrementArticleViewCount(_ context.Context, articleID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.articles[articleID]
	if !ok {
		return ErrNotFound
	}
	a.ViewCount++
	return nil
}
