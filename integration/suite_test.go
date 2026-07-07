// Package integration exercises github.com/libtnb/migrate against real
// databases. It lives in its own module so the parent stays free of
// third-party dependencies. SQLite (pure Go) always runs; Postgres and MySQL
// run when MIGRATE_POSTGRES_DSN / MIGRATE_MYSQL_DSN point at a server, as CI
// does with service containers.
package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/libtnb/migrate"
)

// appSchema registers the reference migrations used across all engines.
func appSchema() *migrate.Collection {
	c := migrate.NewCollection()
	c.Add("001_create_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.String("name", 100).Comment("display name")
			t.Enum("role", "admin", "member").Default("member")
			t.Timestamps()
			t.Comment("registered accounts")
		})
	})
	c.Add("002_create_posts", func(s *migrate.Schema) {
		s.Create("posts", func(t *migrate.Table) {
			t.ID()
			t.ForeignID("user_id").Constrained().CascadeOnDelete()
			t.String("title")
			t.Index("user_id", "title")
		})
	})
	return c
}

func dropAll(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"posts", "users", "schema_migrations"} {
		if _, err := db.Exec("DROP TABLE IF EXISTS " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func count(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// runEndToEnd drives the full lifecycle — apply, constraint behaviour,
// idempotency, rollback, reset — against a live database.
func runEndToEnd(t *testing.T, db *sql.DB, dialect migrate.Dialect) {
	ctx := context.Background()
	dropAll(t, db)
	c := appSchema()
	m, err := migrate.New(db, dialect, migrate.WithCollection(c))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Apply everything.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The declared schema behaves: defaults fill, uniques reject, foreign
	// keys cascade, enums constrain.
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('a@x.dev', 'A')`)
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('b@x.dev', 'B')`)
	if _, err := db.Exec(`INSERT INTO users (email, name) VALUES ('a@x.dev', 'dup')`); err == nil {
		t.Error("duplicate email should violate the unique index")
	}
	if _, err := db.Exec(`INSERT INTO users (email, name, role) VALUES ('c@x.dev', 'C', 'superuser')`); err == nil {
		t.Error("an enum value outside the declared set should be rejected")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM users WHERE role = 'member'`); got != 2 {
		t.Errorf("default role should apply, member count = %d", got)
	}
	mustExec(t, db, `INSERT INTO posts (user_id, title) VALUES (1, 'hello')`)
	mustExec(t, db, `DELETE FROM users WHERE id = 1`)
	if got := count(t, db, `SELECT COUNT(*) FROM posts`); got != 0 {
		t.Errorf("deleting the user should cascade to posts, count = %d", got)
	}

	// A second Up is a no-op.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("second Up: %v", err)
	}
	sts, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != 2 || !sts[0].Applied || !sts[1].Applied || sts[0].Drifted {
		t.Fatalf("unexpected status: %+v", sts)
	}
	if plans, _ := m.Plan(ctx); len(plans) != 0 {
		t.Errorf("nothing should be pending, got %+v", plans)
	}

	// Roll back the latest batch: both migrations were one batch here, so
	// step once instead.
	if err := m.Rollback(ctx, migrate.Steps(1)); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := db.Exec(`SELECT COUNT(*) FROM posts`); err == nil {
		t.Error("posts should be gone after rolling back 002_create_posts")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM users`); got != 1 {
		t.Error("users must survive rolling back the posts migration")
	}

	// Re-apply, then reset everything.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("re-Up: %v", err)
	}
	if err := m.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, table := range []string{"users", "posts"} {
		if _, err := db.Exec("SELECT COUNT(*) FROM " + table); err == nil {
			t.Errorf("%s should not exist after Reset", table)
		}
	}
	if got := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); got != 0 {
		t.Errorf("records should be empty after Reset, got %d", got)
	}
}

// runChecksumFlow verifies drift detection and repair against a live records
// table: apply, mutate the declaration, observe the warning path, the strict
// failure, and the repair.
func runChecksumFlow(t *testing.T, db *sql.DB, dialect migrate.Dialect) {
	ctx := context.Background()
	dropAll(t, db)

	build := func(length int) *migrate.Collection {
		c := migrate.NewCollection()
		c.Add("001_users", func(s *migrate.Schema) {
			s.Create("users", func(t *migrate.Table) {
				t.ID()
				t.String("email", length)
			})
		})
		return c
	}

	m1, _ := migrate.New(db, dialect, migrate.WithCollection(build(255)))
	if err := m1.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}

	// The same migration, edited after the fact.
	edited := build(191)
	strict, _ := migrate.New(db, dialect, migrate.WithCollection(edited), migrate.WithStrictChecksum())
	if err := strict.Up(ctx); !errors.Is(err, migrate.ErrChecksumMismatch) {
		t.Fatalf("strict Up = %v, want ErrChecksumMismatch", err)
	}
	sts, _ := strict.Status(ctx)
	if len(sts) != 1 || !sts[0].Drifted {
		t.Fatalf("status should report drift: %+v", sts)
	}

	// Repair accepts the edited declaration; strict mode passes afterwards.
	if err := strict.Repair(ctx); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if err := strict.Up(ctx); err != nil {
		t.Fatalf("Up after Repair: %v", err)
	}
}

// runDataMigration verifies Run functions execute inside the migration and
// see prior schema changes.
func runDataMigration(t *testing.T, db *sql.DB, dialect migrate.Dialect) {
	ctx := context.Background()
	dropAll(t, db)

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("name")
			t.String("display_name").Nullable()
		})
		s.Run(func(ctx context.Context, db migrate.DB) error {
			if _, err := db.ExecContext(ctx, `INSERT INTO users (name) VALUES ('seed')`); err != nil {
				return err
			}
			_, err := db.ExecContext(ctx, `UPDATE users SET display_name = name WHERE display_name IS NULL`)
			return err
		})
	}, migrate.WithDown(func(s *migrate.Schema) {
		s.Drop("users")
	}))

	m, _ := migrate.New(db, dialect, migrate.WithCollection(c))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM users WHERE display_name = 'seed'`); got != 1 {
		t.Errorf("data migration should have backfilled, got %d", got)
	}
	if err := m.Rollback(ctx); err != nil {
		t.Fatalf("Rollback via explicit down: %v", err)
	}
}

// runBaseline verifies adopting an existing database without executing.
func runBaseline(t *testing.T, db *sql.DB, dialect migrate.Dialect) {
	ctx := context.Background()
	dropAll(t, db)

	c := appSchema()
	m, _ := migrate.New(db, dialect, migrate.WithCollection(c))
	if err := m.Baseline(ctx); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if _, err := db.Exec(`SELECT COUNT(*) FROM users`); err == nil {
		t.Fatal("Baseline must not create tables")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM schema_migrations WHERE batch = 0`); got != 2 {
		t.Errorf("baselined records = %d, want 2", got)
	}
	if plans, _ := m.Plan(ctx); len(plans) != 0 {
		t.Errorf("nothing should be pending after Baseline, got %+v", plans)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up after Baseline: %v", err)
	}
}
