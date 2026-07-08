package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/go-rio/migrate"
	_ "modernc.org/sqlite" // pure-Go driver, keeps integration CGO-free
)

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "app.db") +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteEndToEnd(t *testing.T)     { runEndToEnd(t, openSQLite(t), migrate.SQLite) }
func TestSQLiteChecksumFlow(t *testing.T) { runChecksumFlow(t, openSQLite(t), migrate.SQLite) }
func TestSQLiteDataMigration(t *testing.T) {
	runDataMigration(t, openSQLite(t), migrate.SQLite)
}
func TestSQLiteBaseline(t *testing.T) { runBaseline(t, openSQLite(t), migrate.SQLite) }

// An Integer AutoIncrement column assigns ids on insert like ID does.
func TestSQLiteAutoIncrement(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_counters", func(s *migrate.Schema) {
		s.Create("counters", func(t *migrate.Table) {
			t.Integer("id").AutoIncrement()
			t.String("name")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('a')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('b')")
	if got := count(t, db, "SELECT id FROM counters WHERE name = 'b'"); got != 2 {
		t.Errorf("second row should get id 2, got %d", got)
	}
}

// A failed migration must leave nothing behind on a transactional-DDL engine:
// no half-created tables, no record, no dirty flag to clear — and fixing the
// migration simply makes the next Up succeed.
func TestSQLiteFailedMigrationLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	bad := migrate.NewCollection()
	bad.Add("001_things", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
		s.Exec("CREATE BROKEN SYNTAX")
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(bad))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err == nil {
		t.Fatal("Up should fail on the broken statement")
	}
	if _, err := db.Exec("SELECT COUNT(*) FROM things"); err == nil {
		t.Error("the transaction should have rolled back the CREATE TABLE")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 0 {
		t.Errorf("no record may exist for the failed migration, got %d", got)
	}

	fixed := migrate.NewCollection()
	fixed.Add("001_things", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
	})
	m2, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(fixed))
	if err != nil {
		t.Fatal(err)
	}
	if err := m2.Up(ctx); err != nil {
		t.Fatalf("Up after fixing the migration: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM things"); got != 0 {
		t.Errorf("things should exist and be empty, got count %d", got)
	}
}

// WithoutTransaction migrations run their statements directly on the
// connection and are still recorded.
func TestSQLiteWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_plain", func(s *migrate.Schema) {
		s.Create("plain", func(t *migrate.Table) { t.ID() })
	}, migrate.WithoutTransaction())
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 1 {
		t.Errorf("the migration should be recorded, got %d rows", got)
	}
}

// Rolling back an alteration reverses each change, including indexes implied
// by column modifiers, which SQLite requires to be dropped before the column.
func TestSQLiteAlterRollback(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	})
	c.Add("002_add_nickname", func(s *migrate.Schema) {
		s.Table("users", func(t *migrate.Table) {
			t.String("nickname").Nullable().Index()
			t.RenameColumn("email", "mail")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (mail, nickname) VALUES ('a@x.dev', 'a')")

	// Both migrations landed in one batch; step back just the alteration.
	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE email = 'a@x.dev'"); got != 1 {
		t.Error("the rename should have reversed and data survived")
	}
	if _, err := db.Exec("SELECT nickname FROM users"); err == nil {
		t.Error("nickname should be dropped by the automatic down")
	}
}

func TestSQLiteRepeatable(t *testing.T) { runRepeatable(t, openSQLite(t), migrate.SQLite) }

// Recreate rebuilds a table with a new shape while keeping its rows: the only
// way to change constraints on SQLite. Rows survive, new constraints enforce,
// and conventional index names are rebuilt for the final table name.
func TestSQLiteRecreate(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('a@x.dev')")
	mustExec(t, db, "INSERT INTO users (email) VALUES ('b@x.dev')")

	c.Add("002_unique_emails", func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.Integer("logins").Default(0).SkipCopy()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email")
		})
	}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreate: %v", err)
	}

	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 2 {
		t.Errorf("rows must survive the rebuild, got %d", got)
	}
	if got := count(t, db, "SELECT id FROM users WHERE email = 'b@x.dev'"); got != 2 {
		t.Errorf("ids must survive the copy, got %d", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE logins = 0"); got != 2 {
		t.Errorf("the skipped column should take its default, got %d", got)
	}
	if _, err := db.Exec("INSERT INTO users (email) VALUES ('a@x.dev')"); err == nil {
		t.Error("the rebuilt unique index should reject duplicates")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'users_email_unique'`); got != 1 {
		t.Error("the rebuilt index should carry the conventional final name")
	}
	if got := count(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%__migrate_new%'`); got != 0 {
		t.Error("no temporary object may survive the rebuild")
	}

	// The explicit down rebuilds the permissive shape.
	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	mustExec(t, db, "INSERT INTO users (email) VALUES ('a@x.dev')")
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE email = 'a@x.dev'"); got != 2 {
		t.Errorf("after rollback duplicates are allowed again, got %d", got)
	}
}

// Fresh drops every table — the drop loop unwinds foreign key dependencies
// (this connection enforces them) — then reruns all migrations from scratch.
func TestSQLiteFresh(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := appSchema()
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('a@x.dev', 'A')`)
	mustExec(t, db, `INSERT INTO posts (user_id, title) VALUES (1, 'hello')`)

	if err := m.Fresh(ctx); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users"); got != 0 {
		t.Errorf("users should be recreated empty, got %d rows", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM posts"); got != 0 {
		t.Errorf("posts should be recreated empty, got %d rows", got)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 2 {
		t.Errorf("all migrations should be re-recorded, got %d", got)
	}
}

// Schema-qualified names work end to end against an attached database.
func TestSQLiteQualifiedNames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sql.Open("sqlite",
		"file:"+filepath.Join(dir, "main.db")+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// A single connection so the ATTACH survives for the whole test.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("ATTACH DATABASE '" + filepath.Join(dir, "aux.db") + "' AS aux"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	c := migrate.NewCollection()
	c.Add("001_aux_items", func(s *migrate.Schema) {
		s.Create("aux.items", func(t *migrate.Table) {
			t.ID()
			t.String("name").Unique()
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO aux.items (name) VALUES ('x')")
	if _, err := db.Exec("INSERT INTO aux.items (name) VALUES ('x')"); err == nil {
		t.Error("the unique index should exist in the attached schema")
	}
	if err := m.Rollback(ctx, 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM sqlite_master WHERE name = 'items'"); got != 0 {
		t.Error("the main schema must stay untouched")
	}
}

// Generated columns compute, checks enforce, and CopyFrom converts data
// through a Recreate — the full v0.3 surface against a real database.
func TestSQLiteGeneratedChecksAndCopyFrom(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	c := migrate.NewCollection()
	c.Add("001_people", func(s *migrate.Schema) {
		s.Create("people", func(t *migrate.Table) {
			t.ID()
			t.String("first")
			t.String("last")
			t.String("full").StoredAs("first || ' ' || last")
			t.String("age") // stringly typed on purpose; fixed below
			t.Check("people_first_nonempty", "length(first) > 0")
		})
	})
	m, err := migrate.New(db, migrate.SQLite, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO people (first, last, age) VALUES ('Ada', 'Lovelace', '36')`)
	if got := count(t, db, `SELECT COUNT(*) FROM people WHERE full = 'Ada Lovelace'`); got != 1 {
		t.Error("the stored generated column should compute")
	}
	if _, err := db.Exec(`INSERT INTO people (first, last, age) VALUES ('', 'X', '1')`); err == nil {
		t.Error("the check constraint should reject empty first names")
	}

	c.Add("002_age_integer", func(s *migrate.Schema) {
		s.Recreate("people", func(t *migrate.Table) {
			t.ID()
			t.String("first")
			t.String("last")
			t.String("full").StoredAs("first || ' ' || last")
			t.Integer("age").CopyFrom("CAST(age AS INTEGER)")
			t.Check("people_first_nonempty", "length(first) > 0")
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreate: %v", err)
	}
	if got := count(t, db, `SELECT age + 1 FROM people WHERE first = 'Ada'`); got != 37 {
		t.Errorf("age should be a real integer after CopyFrom cast, got %d", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM people WHERE full = 'Ada Lovelace'`); got != 1 {
		t.Error("the generated column should recompute after the rebuild")
	}
}
