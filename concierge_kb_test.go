package concierge

import (
	"database/sql"
	"errors"
	"testing"
	"time"
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

// TestKB_UpsertsAreIdempotent pins an upsert repeated over a store that already
// holds its rows: repeating it leaves one row per key, carrying the newest
// content, under the identity the first write gave it, with the view count
// IncrementArticleViewCount added in between intact.
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

// TestKB_UpsertsReplaceEveryAuthoredFieldAndKeepTheCounts pins each field the
// Store contract names for the two upserts. A replacing upsert takes every
// authored field from the caller, and keeps the stored id, creation time, view
// count and helpful count; the caller's struct reports what was stored. Each
// field is set on the first write and changed on the second, so a statement or
// a copy that dropped any one of them leaves the first value behind.
func TestKB_UpsertsReplaceEveryAuthoredFieldAndKeepTheCounts(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		first := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing", Description: "first description",
			ParentID: "parent-1", SortOrder: 1, ArticleCount: 3}
		if err := s.UpsertCategory(ctx, first); err != nil {
			t.Fatalf("UpsertCategory (insert): %v", err)
		}
		second := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing and payments",
			Description: "second description", ParentID: "parent-2", SortOrder: 2, ArticleCount: 4}
		if err := s.UpsertCategory(ctx, second); err != nil {
			t.Fatalf("UpsertCategory (replace): %v", err)
		}
		want := KBCategory{ID: first.ID, Partition: "acme", Slug: "billing", Name: "Billing and payments",
			Description: "second description", ParentID: "parent-2", SortOrder: 2, ArticleCount: 4}
		stored, err := s.GetCategoryBySlug(ctx, "acme", "billing")
		if err != nil {
			t.Fatalf("GetCategoryBySlug: %v", err)
		}
		for name, got := range map[string]KBCategory{"stored row": *stored, "caller's struct": *second} {
			if !got.CreatedAt.Equal(first.CreatedAt) {
				t.Errorf("%s: created %v, want the first write's %v", name, got.CreatedAt, first.CreatedAt)
			}
			got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
			if got != want {
				t.Errorf("%s after a replacing UpsertCategory:\n got %+v\nwant %+v", name, got, want)
			}
		}

		draft := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds",
			Content: "first text", ContentHTML: "<p>first text</p>", CategoryID: "category-1",
			CategoryName: "Billing", AuthorID: "author-1", AuthorName: "First Author", Status: "draft",
			Metadata: `{"rev":1}`, HelpfulCount: 5}
		if err := s.UpsertArticle(ctx, draft); err != nil {
			t.Fatalf("UpsertArticle (insert): %v", err)
		}
		if err := s.IncrementArticleViewCount(ctx, draft.ID); err != nil {
			t.Fatalf("IncrementArticleViewCount: %v", err)
		}
		live := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refund policy",
			Content: "second text", ContentHTML: "<p>second text</p>", CategoryID: "category-2",
			CategoryName: "Billing and payments", AuthorID: "author-2", AuthorName: "Second Author",
			Status: ArticleStatusPublished, Metadata: `{"rev":2}`, HelpfulCount: 9, ViewCount: 7}
		if err := s.UpsertArticle(ctx, live); err != nil {
			t.Fatalf("UpsertArticle (replace): %v", err)
		}
		wantArticle := KBArticle{ID: draft.ID, Partition: "acme", CategorySlug: "billing", Slug: "refunds",
			Title: "Refund policy", Content: "second text", ContentHTML: "<p>second text</p>",
			CategoryID: "category-2", CategoryName: "Billing and payments", AuthorID: "author-2",
			AuthorName: "Second Author", Status: ArticleStatusPublished, Metadata: `{"rev":2}`,
			ViewCount: 1, HelpfulCount: 5}
		read, err := s.GetArticleBySlug(ctx, "acme", "billing", "refunds")
		if err != nil {
			t.Fatalf("GetArticleBySlug: %v", err)
		}
		for name, got := range map[string]KBArticle{"stored row": *read, "caller's struct": *live} {
			if !got.CreatedAt.Equal(draft.CreatedAt) {
				t.Errorf("%s: created %v, want the first write's %v", name, got.CreatedAt, draft.CreatedAt)
			}
			got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
			if got != wantArticle {
				t.Errorf("%s after a replacing UpsertArticle:\n got %+v\nwant %+v", name, got, wantArticle)
			}
		}
	})
}

// TestKB_AnArticleSlugWithoutItsCategoryAnswersOneArticle pins GetArticleBySlug
// with an empty category slug. The article key includes the category, so one
// slug can be published once in each category; the read answers the published
// one whose category slug sorts first, on both stores and on every call. The
// draft in the first category is skipped, because the public reads see
// published articles only. Among the published three, the one whose category
// sorts first is neither first nor last by title nor written first or last, so
// a read ordered by title either way (ListArticles orders by title) or by the
// order rows were written answers another category.
func TestKB_AnArticleSlugWithoutItsCategoryAnswersOneArticle(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		for _, a := range []*KBArticle{
			{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "A refund on a bill", Status: ArticleStatusPublished},
			{Partition: "acme", CategorySlug: "account", Slug: "refunds", Title: "Refunds to your account", Status: ArticleStatusPublished},
			{Partition: "acme", CategorySlug: "zebra", Slug: "refunds", Title: "Zebra refunds", Status: ArticleStatusPublished},
			{Partition: "acme", CategorySlug: "aardvark", Slug: "refunds", Title: "Aardvark refunds", Status: "draft"},
		} {
			if err := s.CreateArticle(ctx, a); err != nil {
				t.Fatalf("CreateArticle(%s/%s): %v", a.CategorySlug, a.Slug, err)
			}
		}
		seen := map[string]int{}
		for i := 0; i < 64; i++ {
			got, err := s.GetArticleBySlug(ctx, "acme", "", "refunds")
			if err != nil {
				t.Fatalf("GetArticleBySlug with no category: %v", err)
			}
			seen[got.CategorySlug]++
		}
		if len(seen) != 1 || seen["account"] != 64 {
			t.Errorf("an article slug read without its category must answer the published article whose category sorts first, every time; got %v", seen)
		}
	})
}

// TestKB_ACallerSuppliedIDAnotherRowHoldsIsRefused pins the four KB writes when
// the caller names an id that a row under another key already holds. SQLStore's
// primary key refuses such a row, so MemStore refuses it too, and both answer
// ErrDuplicateID rather than one replacing the other row and the other
// returning its driver's constraint text. The row holding the id is unchanged.
func TestKB_ACallerSuppliedIDAnotherRowHoldsIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		billing := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing"}
		if err := s.CreateCategory(ctx, billing); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		refunds := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds",
			Status: ArticleStatusPublished}
		if err := s.CreateArticle(ctx, refunds); err != nil {
			t.Fatalf("CreateArticle: %v", err)
		}
		if err := s.CreateCategory(ctx, &KBCategory{ID: billing.ID, Partition: "acme", Slug: "account", Name: "Account"}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("CreateCategory naming a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.UpsertCategory(ctx, &KBCategory{ID: billing.ID, Partition: "acme", Slug: "account", Name: "Account"}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("UpsertCategory inserting under a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.CreateArticle(ctx, &KBArticle{ID: refunds.ID, Partition: "acme", CategorySlug: "billing", Slug: "invoices",
			Title: "Invoices", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("CreateArticle naming a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.UpsertArticle(ctx, &KBArticle{ID: refunds.ID, Partition: "acme", CategorySlug: "billing", Slug: "invoices",
			Title: "Invoices", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("UpsertArticle inserting under a held id: want ErrDuplicateID, got %v", err)
		}
		// A refused write leaves the caller's struct as it was given, so a caller
		// holding either store never reads an id that names no stored row.
		taken := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing again"}
		if err := s.CreateCategory(ctx, taken); !errors.Is(err, ErrDuplicateSlug) || taken.ID != "" || !taken.CreatedAt.IsZero() || !taken.UpdatedAt.IsZero() {
			t.Errorf("a create refused for its slug must leave the struct as given; got %+v err=%v", taken, err)
		}
		takenArticle := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds again"}
		if err := s.CreateArticle(ctx, takenArticle); !errors.Is(err, ErrDuplicateSlug) || takenArticle.ID != "" || !takenArticle.CreatedAt.IsZero() || !takenArticle.UpdatedAt.IsZero() {
			t.Errorf("an article create refused for its slug must leave the struct as given; got %+v err=%v", takenArticle, err)
		}
		heldUpsert := &KBArticle{ID: refunds.ID, Partition: "acme", CategorySlug: "billing", Slug: "credits", Title: "Credits"}
		if err := s.UpsertArticle(ctx, heldUpsert); !errors.Is(err, ErrDuplicateID) || heldUpsert.ID != refunds.ID || !heldUpsert.CreatedAt.IsZero() || !heldUpsert.UpdatedAt.IsZero() {
			t.Errorf("an upsert refused for its id must leave the struct as given; got %+v err=%v", heldUpsert, err)
		}
		if got, err := s.GetCategoryBySlug(ctx, "acme", "billing"); err != nil || got.ID != billing.ID || got.Name != "Billing" {
			t.Errorf("the category holding the id must be unchanged; got %+v err=%v", got, err)
		}
		if _, err := s.GetCategoryBySlug(ctx, "acme", "account"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a refused category must not be stored; got err=%v", err)
		}
		if got, err := s.ListArticles(ctx, "acme", "billing"); err != nil || len(got) != 1 || got[0].ID != refunds.ID || got[0].Slug != "refunds" {
			t.Errorf("the article holding the id must be the only one, unchanged; got %v err=%v", slugs(got), err)
		}
		// Where the key is taken as well, the slug answers first on both stores:
		// a create is refused for its slug, and a replacing upsert keeps the
		// stored row's id whatever the caller names, as the contract says.
		other := &KBArticle{Partition: "acme", CategorySlug: "account", Slug: "refunds", Title: "Account refunds",
			Status: ArticleStatusPublished}
		if err := s.CreateArticle(ctx, other); err != nil {
			t.Fatalf("CreateArticle(account/refunds): %v", err)
		}
		for _, id := range []string{refunds.ID, other.ID} {
			if err := s.CreateArticle(ctx, &KBArticle{ID: id, Partition: "acme", CategorySlug: "billing", Slug: "refunds",
				Title: "Refunds again", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateSlug) {
				t.Errorf("CreateArticle on a taken key naming id %q: want ErrDuplicateSlug, got %v", id, err)
			}
		}
		replacing := &KBArticle{ID: other.ID, Partition: "acme", CategorySlug: "billing", Slug: "refunds",
			Title: "Refund policy", Status: ArticleStatusPublished}
		if err := s.UpsertArticle(ctx, replacing); err != nil || replacing.ID != refunds.ID {
			t.Errorf("a replacing upsert keeps the stored id; got id %q err=%v, want %q", replacing.ID, err, refunds.ID)
		}
		if got, err := s.GetArticleBySlug(ctx, "acme", "account", "refunds"); err != nil || got.ID != other.ID || got.Title != "Account refunds" {
			t.Errorf("the row whose id the replacing upsert named must be unchanged; got %+v err=%v", got, err)
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

// TestInitSchemaRefusesAnExistingTableHoldingADuplicateKey pins the condition
// the slug indexes are built under: over a KB table that already holds two rows
// under one key, the unique index cannot be built, and InitSchema returns that
// error rather than starting a store whose upserts would have no ON CONFLICT
// target.
func TestInitSchemaRefusesAnExistingTableHoldingADuplicateKey(t *testing.T) {
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
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_concierge_kb_articles_slug`); err != nil {
		t.Fatal(err)
	}
	dup := `INSERT INTO concierge_kb_articles (id, partition, category_slug, slug) VALUES (?, 'acme', 'billing', 'refunds')`
	for _, id := range []string{"row-1", "row-2"} {
		if _, err := db.ExecContext(ctx, dup, id); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if err := s.InitSchema(ctx); err == nil {
		t.Error("InitSchema over a table holding a duplicate key must return the index build's error")
	}
}

func slugs(as []*KBArticle) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.CategorySlug+"/"+a.Slug)
	}
	return out
}

// TestKB_AnIDAnotherPartitionsRowHoldsIsRefused pins the four KB writes when
// the id a caller names is held by a row of another partition, under a slug key
// that is free in the caller's partition but taken in the other. The id is one
// namespace across partitions (SQLStore's primary key), so both stores answer
// ErrDuplicateID, and the key that decides "free" is the caller's own
// partition's: reading the slug key without its partition would find the other
// partition's row and report the write as a key conflict instead. Neither
// partition's rows change.
func TestKB_AnIDAnotherPartitionsRowHoldsIsRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		billing := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing"}
		if err := s.CreateCategory(ctx, billing); err != nil {
			t.Fatalf("CreateCategory: %v", err)
		}
		refunds := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds",
			Status: ArticleStatusPublished}
		if err := s.CreateArticle(ctx, refunds); err != nil {
			t.Fatalf("CreateArticle: %v", err)
		}

		if err := s.CreateCategory(ctx, &KBCategory{ID: billing.ID, Partition: "globex", Slug: "billing", Name: "Billing"}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("CreateCategory in another partition naming a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.UpsertCategory(ctx, &KBCategory{ID: billing.ID, Partition: "globex", Slug: "billing", Name: "Billing"}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("UpsertCategory in another partition naming a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.CreateArticle(ctx, &KBArticle{ID: refunds.ID, Partition: "globex", CategorySlug: "billing", Slug: "refunds",
			Title: "Refunds", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("CreateArticle in another partition naming a held id: want ErrDuplicateID, got %v", err)
		}
		if err := s.UpsertArticle(ctx, &KBArticle{ID: refunds.ID, Partition: "globex", CategorySlug: "billing", Slug: "refunds",
			Title: "Refunds", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateID) {
			t.Errorf("UpsertArticle in another partition naming a held id: want ErrDuplicateID, got %v", err)
		}

		if got, err := s.GetCategoryBySlug(ctx, "acme", "billing"); err != nil || got.ID != billing.ID || got.Name != "Billing" {
			t.Errorf("the category holding the id must be unchanged; got %+v err=%v", got, err)
		}
		if got, err := s.ListArticles(ctx, "acme", "billing"); err != nil || len(got) != 1 || got[0].ID != refunds.ID {
			t.Errorf("the article holding the id must be the only one in its partition; got %v err=%v", slugs(got), err)
		}
		if _, err := s.GetCategoryBySlug(ctx, "globex", "billing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("a refused category must not be stored in the other partition; got err=%v", err)
		}
		if got, err := s.ListArticles(ctx, "globex", "billing"); err != nil || len(got) != 0 {
			t.Errorf("a refused article must not be stored in the other partition; got %v err=%v", slugs(got), err)
		}
	})
}
