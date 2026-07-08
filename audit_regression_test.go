package migrate

// Regression tests for the findings of the v0.3 adversarial audit. Each test
// names the failure it pins down; none of these may regress silently.

import (
	"context"
	"strings"
	"testing"
	"time"
)

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
	if err := m.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "posts"`)) != 1 {
		t.Error("the real migration should roll back")
	}
	// Second rollback finds only the baseline left — and must not touch it.
	f.setRecords(base)
	if err := m.Rollback(context.Background()); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Fatal("a plain Rollback must never drop baselined tables")
	}
	if err := m.Rollback(context.Background(), Steps(5)); err != nil {
		t.Fatalf("Steps rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 0 {
		t.Fatal("Steps must never drop baselined tables either")
	}

	// Reset is the documented exception.
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 1 {
		t.Error("Reset still rolls baselined rows back, as documented")
	}
}

// Audit: Repair used to rewrite a drifted repeatable's checksum, silently
// cancelling its pending re-run.
func TestRepairLeavesRepeatablesDue(t *testing.T) {
	f := newFakeDB()
	old := viewCollection("active")
	edited := viewCollection("active AND verified")
	f.setRecords(
		appliedRecord(t, edited, Postgres, "001_users", 1),
		repeatableRecord(t, old, "active_users_view"), // stale: due a re-run
	)
	m := testMigrator(t, f, Postgres, edited)
	if err := m.Repair(context.Background()); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if len(f.loggedContaining("UPDATE \"schema_migrations\" SET checksum")) != 0 {
		t.Fatal("Repair must not rewrite repeatable checksums")
	}
	// The re-run stays due.
	recs := []record{repeatableRecord(t, old, "active_users_view")}
	due, _, err := m.dueRepeatables(recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatal("the edited repeatable must still be due after Repair")
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

// Audit: SQLite ADD COLUMN restrictions that only bite on populated tables
// must fail at compile time.
func TestSQLiteAlterAddColumnGuards(t *testing.T) {
	cases := map[string]struct {
		declare func(*Table)
		want    string
	}{
		"stored generated": {
			func(t *Table) { t.String("summary").StoredAs("substr(body,1,100)") },
			"STORED generated column",
		},
		"use current": {
			func(t *Table) { t.TimestampTz("seen").Nullable().UseCurrent() },
			"non-constant default",
		},
		"default expr": {
			func(t *Table) { t.UUID("token").Nullable().DefaultExpr("hex(randomblob(16))") },
			"non-constant default",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &Schema{}
			s.Table("events", tc.declare)
			_, err := SQLite.compile(s.ops[0])
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected compile error mentioning %q, got: %v", tc.want, err)
			}
		})
	}
	// Virtual generated columns remain addable.
	s := &Schema{}
	s.Table("events", func(t *Table) {
		t.String("kind", 32).VirtualAs("json_extract(meta,'$.kind')").Nullable()
	})
	if _, err := SQLite.compile(s.ops[0]); err != nil {
		t.Fatalf("VIRTUAL generated columns are addable on SQLite: %v", err)
	}
}

// Audit: raw-typed auto-increment must be INTEGER on SQLite.
func TestSQLiteRawAutoIncrementRequiresInteger(t *testing.T) {
	s := &Schema{}
	s.Create("t", func(tb *Table) { tb.Column("id", "BIGINT").AutoIncrement() })
	if _, err := SQLite.compile(s.ops[0]); err == nil || !strings.Contains(err.Error(), "INTEGER") {
		t.Fatalf("BIGINT AUTOINCREMENT must fail at compile time, got: %v", err)
	}
	s = &Schema{}
	s.Create("t", func(tb *Table) { tb.Column("id", "integer").AutoIncrement() })
	if _, err := SQLite.compile(s.ops[0]); err != nil {
		t.Fatalf("a raw integer type (any case) is fine: %v", err)
	}
}

// Audit: Primary combined with Nullable silently produced a NULL-accepting
// primary key on SQLite.
func TestPrimaryNullableIsRejected(t *testing.T) {
	cases := map[string]func(*Schema){
		"column primary": func(s *Schema) {
			s.Create("t", func(tb *Table) { tb.String("code").Primary().Nullable() })
		},
		"composite primary": func(s *Schema) {
			s.Create("t", func(tb *Table) {
				tb.Integer("a").Nullable()
				tb.Integer("b")
				tb.Primary("a", "b")
			})
		},
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			m := migrationOf(t, up)
			if _, err := m.upOps(); err == nil {
				t.Fatal("expected a declaration error")
			}
		})
	}
}

// Audit: cross-schema renames silently stayed in the source schema on
// Postgres and SQLite while MySQL moved the table.
func TestCrossSchemaRenameRefused(t *testing.T) {
	s := &Schema{}
	s.Rename("public.orders", "archive.orders")
	if _, err := Postgres.compile(s.ops[0]); err == nil || !strings.Contains(err.Error(), "SET SCHEMA") {
		t.Fatalf("postgres must refuse cross-schema renames, got: %v", err)
	}
	if _, err := SQLite.compile(s.ops[0]); err == nil {
		t.Fatal("sqlite must refuse cross-schema rename")
	}
	if _, err := MySQL.compile(s.ops[0]); err != nil {
		t.Fatalf("mysql legitimately moves tables across databases: %v", err)
	}
	// Same-schema qualified renames keep working.
	s = &Schema{}
	s.Rename("analytics.a", "analytics.b")
	if _, err := Postgres.compile(s.ops[0]); err != nil {
		t.Fatalf("same-schema rename: %v", err)
	}
}

// Audit: RenameIndex ignored the table's schema on Postgres.
func TestRenameIndexQualified(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("analytics.events", func(t *Table) {
			t.RenameIndex("events_kind_index", "events_category_index")
		})
	})
	assertSQL(t, got, []string{
		`ALTER INDEX "analytics"."events_kind_index" RENAME TO "events_category_index"`,
	})
}

// Codex round 2: Steps(-1) used to fall into the Reset branch, bypassing the
// baseline protection a plain Rollback promises.
func TestStepsRejectsNonPositiveCounts(t *testing.T) {
	f := newFakeDB()
	c := twoTables()
	f.setRecords(appliedRecord(t, c, Postgres, "001_users", 0)) // baselined
	m := testMigrator(t, f, Postgres, c)
	for _, n := range []int{0, -1, -100} {
		if err := m.Rollback(context.Background(), Steps(n)); err == nil ||
			!strings.Contains(err.Error(), "positive count") {
			t.Fatalf("Steps(%d) must fail, got: %v", n, err)
		}
	}
	if len(f.loggedContaining("DROP TABLE")) != 0 {
		t.Fatal("nothing may execute for an invalid Steps count")
	}
	if _, err := m.PlanRollback(context.Background(), Steps(-1)); err == nil {
		t.Fatal("PlanRollback must reject invalid Steps too")
	}
}

// Codex round 2: WithoutTransaction reopened the crash window on a Recreate
// that the MySQL gate had closed — statement-by-statement execution can lose
// the live table between the DROP and the rename.
func TestRecreateRequiresTransaction(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_rebuild", func(s *Schema) {
		s.Recreate("users", func(t *Table) { t.ID() })
	}, WithoutTransaction(), WithDown(func(s *Schema) {}))
	m := testMigrator(t, f, Postgres, c)
	err := m.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires the migration's transaction") {
		t.Fatalf("Recreate without a transaction must fail at compile time, got: %v", err)
	}
	if len(f.loggedContaining("DROP TABLE")) != 0 {
		t.Fatal("nothing destructive may execute")
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

func TestDuplicateColumnDeclarationRejected(t *testing.T) {
	m := migrationOf(t, func(s *Schema) {
		s.Create("t", func(tb *Table) {
			tb.ID()
			tb.String("email")
			tb.String("email")
		})
	})
	if _, err := m.upOps(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate columns must be a declaration error, got: %v", err)
	}
}
