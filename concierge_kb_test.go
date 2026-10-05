package concierge

import (
	"database/sql"
	"errors"
	"testing"
)

// TestKB_PublicReadsServePublishedArticlesOnly pins what a visitor who presents
// no principal can read: ListArticles, SearchArticles and GetArticleBySlug are
// the reads a help centre serves to them, so a stored draft must not surface
// through any of the three. The hidden rows carry three statuses (draft,
// archived, none) so a filter that only drops "draft" fails here as well as one
// that drops nothing.
func TestKB_PublicReadsServePublishedArticlesOnly(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.CreateCategory(ctx, &KBCategory{Partition: "acme", Name: "Billing", Slug: "billing"}); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		published := &KBArticle{Partition: "acme", Title: "Refund policy", Slug: "refunds",
			Content: "how to get a refund", CategorySlug: "billing", Status: ArticleStatusPublished}
		hidden := []*KBArticle{
			{Partition: "acme", Title: "Refund policy (rewrite)", Slug: "refunds-rewrite",
				Content: "refund rules being written", CategorySlug: "billing", Status: "draft"},
			{Partition: "acme", Title: "Refund policy (2019)", Slug: "refunds-2019",
				Content: "the old refund rules", CategorySlug: "billing", Status: "archived"},
			{Partition: "acme", Title: "Refund notes", Slug: "refunds-notes",
				Content: "refund notes with no status", CategorySlug: "billing"},
		}
		for _, a := range append([]*KBArticle{published}, hidden...) {
			if err := s.CreateArticle(ctx, a); err != nil {
				t.Fatalf("CreateArticle(%s): %v", a.Slug, err)
			}
		}
		// The positive control: IncrementArticleViewCount reaches a row by id
		// whatever its status, so a nil answer proves each hidden article is
		// stored and that the reads below omit it for its status alone.
		for _, a := range hidden {
			if err := s.IncrementArticleViewCount(ctx, a.ID); err != nil {
				t.Fatalf("the %q article was not stored, so its absence below would prove nothing: %v", a.Status, err)
			}
		}

		for _, category := range []string{"billing", ""} {
			got, err := s.ListArticles(ctx, "acme", category)
			if err != nil {
				t.Fatalf("ListArticles(%q): %v", category, err)
			}
			if len(got) != 1 || got[0].Slug != published.Slug {
				t.Errorf("ListArticles(%q) must list only the published article; got %v", category, slugs(got))
			}
		}

		found, err := s.SearchArticles(ctx, "acme", "refund")
		if err != nil {
			t.Fatalf("SearchArticles: %v", err)
		}
		if len(found) != 1 || found[0].Slug != published.Slug {
			t.Errorf("SearchArticles must match only the published article; got %v", slugs(found))
		}

		if got, err := s.GetArticleBySlug(ctx, "acme", "billing", published.Slug); err != nil || got.ID != published.ID {
			t.Fatalf("GetArticleBySlug(published): %+v %v", got, err)
		}
		for _, a := range hidden {
			for _, category := range []string{"billing", ""} {
				if got, err := s.GetArticleBySlug(ctx, "acme", category, a.Slug); !errors.Is(err, ErrNotFound) {
					t.Errorf("GetArticleBySlug(%q, %s) on a %q article must be a miss; got %+v err=%v",
						category, a.Slug, a.Status, got, err)
				}
			}
		}
	})
}

// TestKB_SlugsAreUniqueWithinTheirScope pins the keys the upserts write under: a
// category slug once per partition, an article slug once per partition and
// category. The same slug in another partition, or another category, is a
// different row.
func TestKB_SlugsAreUniqueWithinTheirScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		if err := s.CreateCategory(ctx, &KBCategory{Partition: "acme", Name: "Billing", Slug: "billing"}); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		if err := s.CreateCategory(ctx, &KBCategory{Partition: "acme", Name: "Billing again", Slug: "billing"}); !errors.Is(err, ErrDuplicateSlug) {
			t.Errorf("a second category with a taken slug must be refused with ErrDuplicateSlug; got %v", err)
		}
		if err := s.CreateCategory(ctx, &KBCategory{Partition: "globex", Name: "Billing", Slug: "billing"}); err != nil {
			t.Errorf("another partition's slug is its own; got %v", err)
		}
		cats, err := s.ListCategories(ctx, "acme")
		if err != nil || len(cats) != 1 || cats[0].Name != "Billing" {
			t.Errorf("the refused category must leave the first one alone; got %d err=%v", len(cats), err)
		}

		art := func(partition, category, title string) *KBArticle {
			return &KBArticle{Partition: partition, CategorySlug: category, Slug: "refunds", Title: title, Status: ArticleStatusPublished}
		}
		if err := s.CreateArticle(ctx, art("acme", "billing", "Refunds")); err != nil {
			t.Fatalf("CreateArticle: %v", err)
		}
		if err := s.CreateArticle(ctx, art("acme", "billing", "Refunds again")); !errors.Is(err, ErrDuplicateSlug) {
			t.Errorf("a second article with a taken slug in the same category must be refused with ErrDuplicateSlug; got %v", err)
		}
		if err := s.CreateArticle(ctx, art("acme", "account", "Account refunds")); err != nil {
			t.Errorf("the same slug in another category is another article; got %v", err)
		}
		if err := s.CreateArticle(ctx, art("globex", "billing", "Refunds")); err != nil {
			t.Errorf("the same slug in another partition is another article; got %v", err)
		}
		if got, err := s.ListArticles(ctx, "acme", "billing"); err != nil || len(got) != 1 || got[0].Title != "Refunds" {
			t.Errorf("the refused article must leave the first one alone; got %v err=%v", slugs(got), err)
		}
		if got, err := s.ListArticles(ctx, "acme", ""); err != nil || len(got) != 2 {
			t.Errorf("acme holds refunds under two categories; got %v err=%v", slugs(got), err)
		}
	})
}

// TestKB_UpsertsAreIdempotent pins the seeder's write: repeating it leaves one
// row per key, carrying the newest content, under the identity the first write
// gave it, with the counters readers accumulated in between intact.
func TestKB_UpsertsAreIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		first := &KBCategory{Partition: "acme", Name: "Billing", Slug: "billing", SortOrder: 1}
		if err := s.UpsertCategory(ctx, first); err != nil || first.ID == "" {
			t.Fatalf("UpsertCategory (insert): id=%q err=%v", first.ID, err)
		}
		second := &KBCategory{Partition: "acme", Name: "Billing and payments", Slug: "billing", SortOrder: 2}
		if err := s.UpsertCategory(ctx, second); err != nil {
			t.Fatalf("UpsertCategory (replace): %v", err)
		}
		if second.ID != first.ID || !second.CreatedAt.Equal(first.CreatedAt) {
			t.Errorf("a replacing upsert must report the stored row's identity; got id %q created %v, want %q %v",
				second.ID, second.CreatedAt, first.ID, first.CreatedAt)
		}
		again := *second
		again.ID = ""
		if err := s.UpsertCategory(ctx, &again); err != nil {
			t.Fatalf("UpsertCategory (repeat): %v", err)
		}
		cats, err := s.ListCategories(ctx, "acme")
		if err != nil || len(cats) != 1 {
			t.Fatalf("three upserts of one slug must leave one category; got %d err=%v", len(cats), err)
		}
		if cats[0].ID != first.ID || cats[0].Name != "Billing and payments" || cats[0].SortOrder != 2 {
			t.Errorf("the one category must carry the newest content under the first id; got %+v", cats[0])
		}

		draft := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds",
			Content: "first text", Status: "draft"}
		if err := s.UpsertArticle(ctx, draft); err != nil || draft.ID == "" {
			t.Fatalf("UpsertArticle (insert): id=%q err=%v", draft.ID, err)
		}
		if err := s.IncrementArticleViewCount(ctx, draft.ID); err != nil {
			t.Fatalf("IncrementArticleViewCount: %v", err)
		}
		if got, _ := s.ListArticles(ctx, "acme", "billing"); len(got) != 0 {
			t.Fatalf("the draft must not be listed before it is published; got %v", slugs(got))
		}
		live := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refund policy",
			Content: "second text", Status: ArticleStatusPublished}
		if err := s.UpsertArticle(ctx, live); err != nil {
			t.Fatalf("UpsertArticle (replace): %v", err)
		}
		if live.ID != draft.ID || live.ViewCount != 1 {
			t.Errorf("a replacing upsert must keep the stored id and view count; got id %q views %d, want %q 1",
				live.ID, live.ViewCount, draft.ID)
		}
		got, err := s.ListArticles(ctx, "acme", "billing")
		if err != nil || len(got) != 1 {
			t.Fatalf("two upserts of one slug must leave one article; got %v err=%v", slugs(got), err)
		}
		if got[0].ID != draft.ID || got[0].Title != "Refund policy" || got[0].Content != "second text" || got[0].ViewCount != 1 {
			t.Errorf("the one article must carry the newest content, the first id and the accumulated views; got %+v", got[0])
		}
		if a, err := s.GetArticleBySlug(ctx, "acme", "billing", "refunds"); err != nil || a.Content != "second text" {
			t.Errorf("GetArticleBySlug after the publishing upsert: %+v %v", a, err)
		}

		other := &KBArticle{Partition: "acme", CategorySlug: "account", Slug: "refunds", Title: "Account refunds",
			Status: ArticleStatusPublished}
		if err := s.UpsertArticle(ctx, other); err != nil || other.ID == draft.ID {
			t.Fatalf("the same slug in another category is another article: id=%q err=%v", other.ID, err)
		}
		if all, _ := s.ListArticles(ctx, "acme", ""); len(all) != 2 {
			t.Errorf("acme holds refunds under two categories; got %v", slugs(all))
		}
	})
}

// TestInitSchemaMakesSlugsUniqueOverAnExistingTable runs InitSchema over KB
// tables that already exist without the slug indexes. A host runs InitSchema at
// every boot over the same database file, and CREATE TABLE IF NOT EXISTS leaves
// an existing table as it is, so uniqueness has to arrive as an index for such a
// file to refuse a duplicate and for the upserts' ON CONFLICT target to exist.
func TestInitSchemaMakesSlugsUniqueOverAnExistingTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := NewSQLStore(db)
	if err := s.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	var indexes []string
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index'
		AND tbl_name IN ('concierge_kb_categories', 'concierge_kb_articles') AND sql LIKE 'CREATE UNIQUE INDEX%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	rows.Close()
	if len(indexes) != 2 {
		t.Fatalf("InitSchema must build one unique slug index on each KB table; got %v", indexes)
	}
	for _, name := range indexes {
		if _, err := db.ExecContext(ctx, `DROP INDEX `+name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema over tables without the slug indexes: %v", err)
	}
	for _, dup := range []string{
		`INSERT INTO concierge_kb_categories (id, partition, slug) VALUES (?, 'acme', 'billing')`,
		`INSERT INTO concierge_kb_articles (id, partition, category_slug, slug) VALUES (?, 'acme', 'billing', 'refunds')`,
	} {
		if _, err := db.ExecContext(ctx, dup, "row-1"); err != nil {
			t.Fatalf("first row: %v", err)
		}
		if _, err := db.ExecContext(ctx, dup, "row-2"); err == nil {
			t.Errorf("the schema admitted a second row under a taken slug: %s", dup)
		}
	}
}

func slugs(as []*KBArticle) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.CategorySlug+"/"+a.Slug)
	}
	return out
}
