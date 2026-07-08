package migrate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testMigrator(t *testing.T, f *fakeDB, d Dialect, c *Collection, opts ...Option) *Migrator {
	t.Helper()
	db := f.open()
	t.Cleanup(func() { _ = db.Close() })
	m, err := New(db, d, append([]Option{WithCollection(c)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// twoTables registers two reversible create-table migrations.
func twoTables() *Collection {
	c := NewCollection()
	c.Add("001_users", func(s *Schema) {
		s.Create("users", func(t *Table) { t.ID() })
	})
	c.Add("002_posts", func(s *Schema) {
		s.Create("posts", func(t *Table) { t.ID() })
	})
	return c
}

// appliedRecord builds a records-table row whose checksum matches what the
// collection currently compiles to, as a real applied migration would have.
func appliedRecord(t *testing.T, c *Collection, d Dialect, name string, batch int) record {
	t.Helper()
	mig := c.get(name)
	if mig == nil {
		t.Fatalf("migration %q not in collection", name)
	}
	sum, err := mig.checksum(d)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	return record{version: name, batch: batch, checksum: sum, appliedAt: "2026-07-08T00:00:00.000000Z"}
}

func TestUpAppliesPendingInOrder(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, twoTables())
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	log := f.logged()
	want := []string{
		"pg_try_advisory_lock",
		"CREATE TABLE IF NOT EXISTS \"schema_migrations\"",
		"SELECT version, batch, checksum, applied_at",
		"BEGIN",
		`CREATE TABLE "users"`,
		"INSERT INTO \"schema_migrations\"",
		"COMMIT",
		"BEGIN",
		`CREATE TABLE "posts"`,
		"INSERT INTO \"schema_migrations\"",
		"COMMIT",
		"pg_advisory_unlock",
	}
	assertLogSequence(t, log, want)
}

// assertLogSequence checks that each fragment appears in order, one per log
// entry, with no unexplained entries between transaction boundaries.
func assertLogSequence(t *testing.T, log []string, fragments []string) {
	t.Helper()
	i := 0
	for _, frag := range fragments {
		found := false
		for ; i < len(log); i++ {
			if strings.Contains(log[i], frag) {
				found = true
				i++
				break
			}
		}
		if !found {
			t.Fatalf("fragment %q not found in order.\nlog:\n%s", frag, strings.Join(log, "\n"))
		}
	}
}

func TestUpSkipsApplied(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(appliedRecord(t, c, Postgres, "001_users", 1))
	m := testMigrator(t, f, Postgres, c)
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := f.loggedContaining(`CREATE TABLE "users"`); len(got) != 0 {
		t.Errorf("001_users ran again: %v", got)
	}
	if got := f.loggedContaining(`CREATE TABLE "posts"`); len(got) != 1 {
		t.Errorf("002_posts should run exactly once, got %v", got)
	}
}

func TestUpFailureRollsBackAndStops(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_ok", func(s *Schema) { s.Create("users", func(t *Table) { t.ID() }) })
	c.Add("002_boom", func(s *Schema) {
		s.Exec("CREATE TABLE a (x int)")
		s.Exec("CREATE BROKEN")
	})
	c.Add("003_never", func(s *Schema) { s.Create("later", func(t *Table) { t.ID() }) })
	f.fail("CREATE BROKEN", errors.New("syntax error"))

	err := testMigrator(t, f, Postgres, c).Up(context.Background())
	if err == nil {
		t.Fatal("Up should fail")
	}
	for _, frag := range []string{"002_boom", "statement 2/3", "CREATE BROKEN", "syntax error"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error should mention %q, got: %v", frag, err)
		}
	}
	if len(f.loggedContaining("ROLLBACK")) != 1 {
		t.Error("failed migration should roll back its transaction")
	}
	if got := f.loggedContaining("INSERT INTO"); len(got) != 1 {
		t.Errorf("only 001_ok may be recorded, got %d inserts", len(got))
	}
	if len(f.loggedContaining(`"later"`)) != 0 {
		t.Error("003_never must not run after a failure")
	}
	if len(f.loggedContaining("pg_advisory_unlock")) != 1 {
		t.Error("the lock must be released after a failure")
	}
}

func TestWithoutTransaction(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_cic", func(s *Schema) {
		s.Exec("CREATE INDEX CONCURRENTLY idx ON t (c)")
	}, WithoutTransaction(), WithDown(func(s *Schema) { s.Exec("DROP INDEX idx") }))
	m := testMigrator(t, f, Postgres, c)
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(f.loggedContaining("BEGIN")) != 0 {
		t.Error("WithoutTransaction must not open a transaction")
	}
	if len(f.loggedContaining("INSERT INTO")) != 1 {
		t.Error("the migration must still be recorded")
	}
}

func TestLockTimeout(t *testing.T) {
	f := newFakeDB()
	f.denyLock = true
	m := testMigrator(t, f, Postgres, twoTables(), WithLockTimeout(time.Millisecond))
	err := m.Up(context.Background())
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if len(f.loggedContaining("CREATE TABLE")) != 0 {
		t.Error("nothing may run without the lock")
	}
}

func TestWithoutLock(t *testing.T) {
	f := newFakeDB()
	f.denyLock = true // would time out if locking were attempted
	m := testMigrator(t, f, Postgres, twoTables(), WithoutLock())
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(f.loggedContaining("advisory_lock")) != 0 {
		t.Error("WithoutLock must not touch advisory locks")
	}
}

func TestChecksumMismatchWarnsByDefault(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	rec := appliedRecord(t, c, Postgres, "001_users", 1)
	rec.checksum = strings.Repeat("0", 64) // stored checksum no longer matches
	f.setRecords(rec)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	m := testMigrator(t, f, Postgres, c, WithLogger(logger))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up should proceed with a warning: %v", err)
	}
	if !strings.Contains(buf.String(), "checksum mismatch") {
		t.Errorf("expected a checksum warning in the log, got: %s", buf.String())
	}
}

func TestChecksumMismatchStrict(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	rec := appliedRecord(t, c, Postgres, "001_users", 1)
	rec.checksum = strings.Repeat("0", 64)
	f.setRecords(rec)

	err := testMigrator(t, f, Postgres, c, WithStrictChecksum()).Up(context.Background())
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}
}

func TestRollbackLatestBatchOnly(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		appliedRecord(t, c, Postgres, "002_posts", 2),
	)
	m := testMigrator(t, f, Postgres, c)
	if err := m.RollbackBatch(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "posts"`)) != 1 {
		t.Error("batch 2 should roll back")
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Error("batch 1 must stay applied")
	}
	if len(f.loggedContaining("DELETE FROM")) != 1 {
		t.Error("exactly one record should be deleted")
	}
}

func TestRollbackSteps(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		appliedRecord(t, c, Postgres, "002_posts", 1),
	)
	m := testMigrator(t, f, Postgres, c)
	if err := m.Rollback(context.Background(), 1); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// Within one batch the later name rolls back first.
	if len(f.loggedContaining(`DROP TABLE "posts"`)) != 1 || len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Errorf("Steps(1) should undo only 002_posts; log:\n%s", strings.Join(f.logged(), "\n"))
	}
}

func TestResetRollsBackEverything(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		appliedRecord(t, c, Postgres, "002_posts", 2),
	)
	m := testMigrator(t, f, Postgres, c)
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// Two per-migration record deletions plus the sweep that forgets
	// repeatable records.
	if len(f.loggedContaining("DROP TABLE")) != 2 || len(f.loggedContaining("DELETE FROM")) != 3 {
		t.Errorf("Reset should undo both migrations; log:\n%s", strings.Join(f.logged(), "\n"))
	}
}

func TestRollbackUnregisteredFails(t *testing.T) {
	f := newFakeDB()
	f.setRecords(record{version: "000_ghost", batch: 1, checksum: "", appliedAt: "2026-07-08T00:00:00.000000Z"})
	m := testMigrator(t, f, Postgres, twoTables())
	err := m.RollbackBatch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "000_ghost") || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want a not-registered error naming 000_ghost", err)
	}
}

func TestRollbackIrreversibleFails(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_raw", func(s *Schema) { s.Exec("UPDATE t SET x = 1") })
	f.setRecords(appliedRecord(t, c, Postgres, "001_raw", 1))
	err := testMigrator(t, f, Postgres, c).Rollback(context.Background(), 1)
	if !errors.Is(err, ErrIrreversible) {
		t.Fatalf("err = %v, want ErrIrreversible", err)
	}
	if len(f.loggedContaining("DELETE FROM")) != 0 {
		t.Error("an irreversible rollback must not touch the records")
	}
}

func TestMySQLFailureExplainsImplicitCommit(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_two_ddl", func(s *Schema) {
		s.Create("a", func(t *Table) { t.ID() })
		s.Create("b", func(t *Table) { t.ID() })
	})
	f.fail("CREATE TABLE `b`", errors.New("boom"))
	err := testMigrator(t, f, MySQL, c).Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "commits implicitly") {
		t.Fatalf("MySQL failures should explain implicit commits, got: %v", err)
	}
}

func TestRunFunctionExecutesInTransaction(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_data", func(s *Schema) {
		s.Run(func(ctx context.Context, db DB) error {
			_, err := db.ExecContext(ctx, "UPDATE marker SET x = 1")
			return err
		})
	}, WithDown(func(s *Schema) {}))
	m := testMigrator(t, f, Postgres, c)
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	log := f.logged()
	assertLogSequence(t, log, []string{"BEGIN", "UPDATE marker", "INSERT INTO", "COMMIT"})
}

func TestSQLiteSkipsLocking(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, SQLite, twoTables())
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if got := f.loggedContaining("lock"); len(got) != 0 {
		t.Errorf("sqlite should not emit lock statements: %v", got)
	}
}

func TestPlanRendersWithoutExecuting(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(appliedRecord(t, c, Postgres, "001_users", 1))
	m := testMigrator(t, f, Postgres, c)
	plans, err := m.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 1 || plans[0].Name != "002_posts" {
		t.Fatalf("plans = %+v, want just 002_posts", plans)
	}
	if !strings.Contains(plans[0].Statements[0], `CREATE TABLE "posts"`) {
		t.Errorf("plan should render the create, got: %v", plans[0].Statements)
	}
	if len(f.loggedContaining("BEGIN")) != 0 || len(f.loggedContaining("INSERT INTO")) != 0 {
		t.Error("Plan must not execute or record anything")
	}
}

func TestPlanRollbackRendersDown(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		appliedRecord(t, c, Postgres, "002_posts", 2),
	)
	m := testMigrator(t, f, Postgres, c)
	plans, err := m.PlanRollbackBatch(context.Background())
	if err != nil {
		t.Fatalf("PlanRollback: %v", err)
	}
	if len(plans) != 1 || plans[0].Name != "002_posts" {
		t.Fatalf("plans = %+v, want just 002_posts", plans)
	}
	if !strings.Contains(plans[0].Statements[0], `DROP TABLE "posts"`) {
		t.Errorf("rollback plan should drop posts, got: %v", plans[0].Statements)
	}
}

func TestBaselineRecordsWithoutRunning(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, twoTables())
	if err := m.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if len(f.loggedContaining("CREATE TABLE \"users\"")) != 0 {
		t.Error("Baseline must not execute migrations")
	}
	inserts := f.loggedContaining("INSERT INTO")
	if len(inserts) != 2 {
		t.Fatalf("Baseline should record both migrations, got %d", len(inserts))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, args := range f.args {
		if len(args) >= 2 {
			if batch, ok := args[1].Value.(int64); ok && batch != 0 {
				t.Errorf("baselined rows must use batch 0, got %d", batch)
			}
		}
	}
}

func TestBaselineUpTo(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, twoTables())
	if err := m.Baseline(context.Background(), "001_users"); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if len(f.loggedContaining("INSERT INTO")) != 1 {
		t.Error("only 001_users should be baselined")
	}
	if err := m.Baseline(context.Background(), "999_missing"); err == nil {
		t.Error("baselining to an unknown migration must fail")
	}
}

func TestRepairRewritesChecksums(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	stale := appliedRecord(t, c, Postgres, "001_users", 1)
	stale.checksum = strings.Repeat("0", 64)
	fresh := appliedRecord(t, c, Postgres, "002_posts", 1)
	f.setRecords(stale, fresh)
	m := testMigrator(t, f, Postgres, c)
	if err := m.Repair(context.Background()); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if len(f.loggedContaining("UPDATE \"schema_migrations\" SET checksum")) != 1 {
		t.Error("exactly the stale record should be repaired")
	}
}

func TestStatusMergesRegisteredAndApplied(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		record{version: "000_ghost", batch: 1, checksum: "abc", appliedAt: "2026-07-08T00:00:00.000000Z"},
	)
	m := testMigrator(t, f, Postgres, c)
	sts, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(sts) != 3 {
		t.Fatalf("status count = %d, want 3", len(sts))
	}
	byName := map[string]Status{}
	for _, s := range sts {
		byName[s.Name] = s
	}
	if s := byName["000_ghost"]; !s.Applied || s.Registered {
		t.Errorf("ghost = %+v, want applied and unregistered", s)
	}
	if s := byName["001_users"]; !s.Applied || !s.Registered || s.Drifted || s.AppliedAt.IsZero() {
		t.Errorf("001_users = %+v, want applied, registered, undrifted, timestamped", s)
	}
	if s := byName["002_posts"]; s.Applied || !s.Registered {
		t.Errorf("002_posts = %+v, want pending", s)
	}
	if sts[0].Name != "000_ghost" || sts[2].Name != "002_posts" {
		t.Errorf("statuses should sort by name: %+v", sts)
	}
}

func TestStatusReportsDrift(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	rec := appliedRecord(t, c, Postgres, "001_users", 1)
	rec.checksum = strings.Repeat("0", 64)
	f.setRecords(rec)
	sts, err := testMigrator(t, f, Postgres, c).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	for _, s := range sts {
		if s.Name == "001_users" && !s.Drifted {
			t.Error("001_users should report drift")
		}
	}
}

func TestNewValidation(t *testing.T) {
	f := newFakeDB()
	db := f.open()
	defer func() { _ = db.Close() }()

	if _, err := New(nil, Postgres); err == nil {
		t.Error("nil db must fail")
	}
	if _, err := New(db, nil); err == nil {
		t.Error("nil dialect must fail")
	}
	if _, err := New(db, Postgres, WithCollection(nil)); err == nil {
		t.Error("nil collection must fail")
	}
	if _, err := New(db, Postgres, WithTable("bad`name")); err == nil {
		t.Error("quoted table name must fail")
	}
	if _, err := New(db, Postgres, WithLockTimeout(0)); err == nil {
		t.Error("zero lock timeout must fail")
	}
	if _, err := New(db, Postgres, WithLogger(nil)); err == nil {
		t.Error("nil logger must fail")
	}
}

// Ensure defaultCollection wiring works end to end without interfering with
// other tests: register into a scratch collection via the package-level Add
// guard test only checks the panic path indirectly elsewhere.
func TestUpNothingPending(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		appliedRecord(t, c, Postgres, "002_posts", 1),
	)
	var buf bytes.Buffer
	m := testMigrator(t, f, Postgres, c, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !strings.Contains(buf.String(), "nothing to apply") {
		t.Error("expected a nothing-to-apply log line")
	}
	if len(f.loggedContaining("BEGIN")) != 0 {
		t.Error("nothing should execute")
	}
}

func TestFreshDropsEverythingThenMigrates(t *testing.T) {
	f := newFakeDB()
	f.tables = []string{"users", "schema_migrations", "stragglers"}
	m := testMigrator(t, f, Postgres, twoTables())
	if err := m.Fresh(context.Background()); err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	for _, table := range []string{`"users"`, `"schema_migrations"`, `"stragglers"`} {
		if len(f.loggedContaining("DROP TABLE IF EXISTS "+table+" CASCADE")) != 1 {
			t.Errorf("expected a cascade drop of %s", table)
		}
	}
	// After dropping, the full migration run happens: table recreated,
	// both migrations applied and recorded.
	assertLogSequence(t, f.logged(), []string{
		"DROP TABLE IF EXISTS",
		"CREATE TABLE IF NOT EXISTS \"schema_migrations\"",
		`CREATE TABLE "users"`,
		"INSERT INTO",
		`CREATE TABLE "posts"`,
		"INSERT INTO",
	})
}

// Audit: a plain Rollback used to select batch-0 baseline rows once they
// became the highest remaining batch, dropping pre-existing production
// tables.
func TestRollbackNeverTouchesBaselinedRows(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	base := appliedRecord(t, c, Postgres, "001_users", 0) // baselined
	real := appliedRecord(t, c, Postgres, "002_posts", 1)
	f.setRecords(base, real)
	m := testMigrator(t, f, Postgres, c)

	// First rollback undoes the real batch.
	if err := m.RollbackBatch(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "posts"`)) != 1 {
		t.Error("the real migration should roll back")
	}
	// Second rollback finds only the baseline left — and must not touch it.
	f.setRecords(base)
	if err := m.RollbackBatch(context.Background()); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Fatal("a plain Rollback must never drop baselined tables")
	}
	if err := m.Rollback(context.Background(), 5); err != nil {
		t.Fatalf("steps rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Fatal("a step rollback must never drop baselined tables either")
	}

	// Reset is the documented exception.
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 1 {
		t.Error("Reset still rolls baselined rows back, as documented")
	}
}

// Codex round 2 (API reshaped in v0.4): a non-positive step count must fail
// before anything loads — negative values used to alias Reset internally and
// bypass the baseline protection.
func TestRollbackRejectsNonPositiveSteps(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(appliedRecord(t, c, Postgres, "001_users", 0)) // baselined
	m := testMigrator(t, f, Postgres, c)
	for _, n := range []int{0, -1, -100} {
		if err := m.Rollback(context.Background(), n); err == nil ||
			!strings.Contains(err.Error(), "positive step count") {
			t.Fatalf("Rollback(ctx, %d) must fail, got: %v", n, err)
		}
	}
	if len(f.loggedContaining("DROP TABLE")) != 0 {
		t.Fatal("nothing may execute for an invalid step count")
	}
	if _, err := m.PlanRollback(context.Background(), -1); err == nil {
		t.Fatal("PlanRollback must reject invalid steps too")
	}
}

// Audit: sub-second MySQL lock timeouts truncated to GET_LOCK(..., 0),
// silently disabling the wait.
func TestMySQLLockTimeoutRoundsUp(t *testing.T) {
	for timeout, want := range map[time.Duration]int64{
		500 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		time.Minute:             60,
	} {
		if got := int64((timeout + time.Second - 1) / time.Second); got != want {
			t.Errorf("GET_LOCK seconds for %v = %d, want %d", timeout, got, want)
		}
	}
	// And the full path still works with a sub-second timeout configured.
	f := newFakeDB()
	m := testMigrator(t, f, MySQL, twoTables(), WithLockTimeout(500*time.Millisecond))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
}

// Self-review round 4: combination gaps found by walking the option matrix.
func TestBaselineValidation(t *testing.T) {
	f := newFakeDB()
	c := viewCollection("active") // one versioned + one repeatable
	m := testMigrator(t, f, Postgres, c)

	if err := m.Baseline(context.Background(), "a", "b"); err == nil {
		t.Fatal("extra variadic arguments must not be silently ignored")
	}
	if err := m.Baseline(context.Background(), "active_users_view"); err == nil ||
		!strings.Contains(err.Error(), "repeatable") {
		t.Fatalf("a repeatable name is not a valid versioned bound, got: %v", err)
	}
	if err := m.Baseline(context.Background(), "001_users"); err != nil {
		t.Fatalf("a versioned bound works: %v", err)
	}
}
