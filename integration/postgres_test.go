package integration

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/libtnb/migrate"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// openPostgres connects to MIGRATE_POSTGRES_DSN or skips, e.g.
// postgres://postgres:postgres@localhost:5432/migrate_test?sslmode=disable
func openPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_POSTGRES_DSN not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPostgresEndToEnd(t *testing.T)     { runEndToEnd(t, openPostgres(t), migrate.Postgres) }
func TestPostgresChecksumFlow(t *testing.T) { runChecksumFlow(t, openPostgres(t), migrate.Postgres) }
func TestPostgresDataMigration(t *testing.T) {
	runDataMigration(t, openPostgres(t), migrate.Postgres)
}
func TestPostgresBaseline(t *testing.T) { runBaseline(t, openPostgres(t), migrate.Postgres) }

// Replicas racing at deploy time: two migrators with separate connection
// pools run Up concurrently. The advisory lock serializes them; both succeed
// and every migration applies exactly once.
func TestPostgresConcurrentMigrators(t *testing.T) {
	db1 := openPostgres(t)
	db2 := openPostgres(t)
	dropAll(t, db1)

	run := func(db *sql.DB) error {
		m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(appSchema()))
		if err != nil {
			return err
		}
		return m.Up(context.Background())
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, db := range []*sql.DB{db1, db2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = run(db)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}
	if got := count(t, db1, "SELECT COUNT(*) FROM schema_migrations"); got != 2 {
		t.Errorf("each migration must apply exactly once, records = %d", got)
	}
}

// A failed migration rolls back atomically on Postgres: transactional DDL is
// the whole point of preferring it for migrations.
func TestPostgresFailedMigrationLeavesNoTrace(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)

	c := migrate.NewCollection()
	c.Add("001_bad", func(s *migrate.Schema) {
		s.Create("things", func(t *migrate.Table) { t.ID() })
		s.Exec("SELECT 1/0") // fails after the create
	})
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err == nil {
		t.Fatal("Up should fail")
	}
	if _, err := db.Exec("SELECT COUNT(*) FROM things"); err == nil {
		t.Error("the transaction should have rolled back the CREATE TABLE")
	}
	if got := count(t, db, "SELECT COUNT(*) FROM schema_migrations"); got != 0 {
		t.Errorf("no record may exist, got %d", got)
	}
}
