package concierge

import (
	"database/sql"
	"errors"
	"testing"
)

// TestSQLStore_AKBWriteRefusedForAnotherReasonReturnsTheEnginesError pins which
// failed KB writes SQLStore answers with ErrDuplicateID. The sentinel answers
// the write the primary key refuses: an insert under a key no row holds, naming
// an id a row holds, the write MemStore refuses before it stores anything. Every
// other failed write returns the engine's error, whichever row holds the id the
// caller named: a caller that reads a row back and upserts it again names that
// row's own id, and a replacing upsert may name another row's id while its key
// resolves the write against the row that holds the key. query_only mode makes
// every write fail for one reason, and the error a plain INSERT gets in that
// mode is the one each of those writes must return.
func TestSQLStore_AKBWriteRefusedForAnotherReasonReturnsTheEnginesError(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // query_only is per connection
	t.Cleanup(func() { db.Close() })
	s := NewSQLStore(db)
	if err := s.InitSchema(ctx); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	billing := &KBCategory{Partition: "acme", Slug: "billing", Name: "Billing"}
	if err := s.UpsertCategory(ctx, billing); err != nil {
		t.Fatalf("UpsertCategory: %v", err)
	}
	refunds := &KBArticle{Partition: "acme", CategorySlug: "billing", Slug: "refunds", Title: "Refunds",
		Status: ArticleStatusPublished}
	if err := s.UpsertArticle(ctx, refunds); err != nil {
		t.Fatalf("UpsertArticle: %v", err)
	}
	other := &KBArticle{Partition: "acme", CategorySlug: "account", Slug: "refunds", Title: "Account refunds",
		Status: ArticleStatusPublished}
	if err := s.CreateArticle(ctx, other); err != nil {
		t.Fatalf("CreateArticle: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		t.Fatal(err)
	}
	_, refused := db.ExecContext(ctx, `INSERT INTO concierge_kb_categories (id) VALUES ('probe')`)
	if refused == nil {
		t.Fatal("query_only mode admitted an INSERT, so no write below fails for that reason")
	}

	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"UpsertCategory of a stored row under its own id", func() error {
			c := *billing
			return s.UpsertCategory(ctx, &c)
		}},
		{"UpsertArticle of a stored row under its own id", func() error {
			a := *refunds
			return s.UpsertArticle(ctx, &a)
		}},
		{"UpsertArticle under a taken key naming another row's id", func() error {
			return s.UpsertArticle(ctx, &KBArticle{ID: other.ID, Partition: "acme", CategorySlug: "billing",
				Slug: "refunds", Title: "Refund policy", Status: ArticleStatusPublished})
		}},
		{"CreateCategory under its own key and id", func() error {
			return s.CreateCategory(ctx, &KBCategory{ID: billing.ID, Partition: "acme", Slug: "billing", Name: "Billing"})
		}},
		{"CreateArticle under a taken key naming another row's id", func() error {
			return s.CreateArticle(ctx, &KBArticle{ID: other.ID, Partition: "acme", CategorySlug: "billing",
				Slug: "refunds", Title: "Refunds again", Status: ArticleStatusPublished})
		}},
		{"UpsertCategory of a new key with no id", func() error {
			return s.UpsertCategory(ctx, &KBCategory{Partition: "acme", Slug: "account", Name: "Account"})
		}},
	} {
		if err := tc.write(); err == nil || errors.Is(err, ErrDuplicateID) || err.Error() != refused.Error() {
			t.Errorf("%s in query_only mode: want the engine's %q, got %v", tc.name, refused, err)
		}
	}

	// The insert the primary key refuses is refused under every state of the
	// store, so it keeps the answer MemStore gives it. The article's slug is held
	// in two other categories, so only the whole key, category included, tells
	// that no row holds the key this write names.
	if err := s.CreateArticle(ctx, &KBArticle{ID: refunds.ID, Partition: "acme", CategorySlug: "orders",
		Slug: "refunds", Title: "Order refunds", Status: ArticleStatusPublished}); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("CreateArticle under a free key naming a held id: want ErrDuplicateID, got %v", err)
	}
	if err := s.UpsertCategory(ctx, &KBCategory{ID: billing.ID, Partition: "acme", Slug: "account",
		Name: "Account"}); !errors.Is(err, ErrDuplicateID) {
		t.Errorf("UpsertCategory inserting under a held id: want ErrDuplicateID, got %v", err)
	}
}
