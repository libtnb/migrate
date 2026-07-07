package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/libtnb/migrate"
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
	if err := m.Rollback(ctx, migrate.Steps(1)); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := count(t, db, "SELECT COUNT(*) FROM users WHERE email = 'a@x.dev'"); got != 1 {
		t.Error("the rename should have reversed and data survived")
	}
	if _, err := db.Exec("SELECT nickname FROM users"); err == nil {
		t.Error("nickname should be dropped by the automatic down")
	}
}
