package integration

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"github.com/go-rio/migrate"

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

func TestPostgresRepeatable(t *testing.T) { runRepeatable(t, openPostgres(t), migrate.Postgres) }

// Audit regression: Recreate used to fail on Postgres for any table-level
// primary key (the temp table claimed the live pkey's backing-index name),
// and left the identity sequence behind the copied rows.
func TestPostgresRecreate(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)
	if _, err := db.Exec("DROP TABLE IF EXISTS counters"); err != nil {
		t.Fatal(err)
	}

	c := migrate.NewCollection()
	c.Add("001_orders", func(s *migrate.Schema) {
		s.Create("orders", func(t *migrate.Table) {
			t.Integer("code").Primary() // table-level named PK: the collision case
			t.String("state")
		})
	})
	c.Add("002_counters", func(s *migrate.Schema) {
		s.Create("counters", func(t *migrate.Table) {
			t.ID() // identity: the sequence case
			t.String("name")
		})
	})
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, "INSERT INTO orders (code, state) VALUES (1, 'open')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('a')")
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('b')")

	c.Add("003_rebuild", func(s *migrate.Schema) {
		s.Recreate("orders", func(t *migrate.Table) {
			t.Integer("code").Primary()
			t.Enum("state", "open", "closed")
		})
		s.Recreate("counters", func(t *migrate.Table) {
			t.ID()
			t.String("name").Unique()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up with recreates: %v", err) // pre-fix: relation "orders_pkey" already exists
	}

	// The PK carries its conventional name after the swap.
	if got := count(t, db, `SELECT COUNT(*) FROM pg_constraint WHERE conname = 'orders_pkey'`); got != 1 {
		t.Errorf("orders_pkey should exist after the rebuild, got %d", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM pg_constraint WHERE conname LIKE '%__migrate_new%'`); got != 0 {
		t.Error("no constraint may keep the temporary name")
	}
	// The identity sequence advanced past the copied rows: the next insert
	// must not collide (pre-fix: duplicate key on counters_pkey).
	mustExec(t, db, "INSERT INTO counters (name) VALUES ('c')")
	if got := count(t, db, "SELECT MAX(id) FROM counters"); got != 3 {
		t.Errorf("the new row should take id 3, max = %d", got)
	}
	mustExec(t, db, "DROP TABLE IF EXISTS counters")
}

// Codex round 2: recreating a parent table referenced by child foreign keys
// is impossible on Postgres (definition-level dependency). The failure must
// be clean — transaction rolled back, original table and data intact.
func TestPostgresRecreateReferencedParentFailsCleanly(t *testing.T) {
	ctx := context.Background()
	db := openPostgres(t)
	dropAll(t, db)

	c := appSchema() // users + posts (posts.user_id → users.id)
	m, err := migrate.New(db, migrate.Postgres, migrate.WithCollection(c))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	mustExec(t, db, `INSERT INTO users (email, name) VALUES ('a@x.dev', 'A')`)

	c.Add("003_rebuild_users", func(s *migrate.Schema) {
		s.Recreate("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.String("name", 100)
			t.Enum("role", "admin", "member").Default("member")
			t.Timestamps()
		})
	}, migrate.WithDown(func(s *migrate.Schema) {}))
	if err := m.Up(ctx); err == nil {
		t.Fatal("recreating a referenced parent must fail on Postgres")
	}
	// Clean failure: table, data and records intact; the failed migration
	// unrecorded so a fixed version can run.
	if got := count(t, db, `SELECT COUNT(*) FROM users`); got != 1 {
		t.Errorf("users must survive the failed rebuild, got %d rows", got)
	}
	if got := count(t, db, `SELECT COUNT(*) FROM schema_migrations`); got != 2 {
		t.Errorf("the failed migration must not be recorded, got %d records", got)
	}
}
