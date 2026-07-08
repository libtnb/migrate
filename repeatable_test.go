package migrate

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// viewCollection returns one versioned migration plus a repeatable view whose
// definition is parameterised, so tests can "edit" it between runs.
func viewCollection(where string) *Collection {
	c := NewCollection()
	c.Add("001_users", func(s *Schema) {
		s.Create("users", func(t *Table) { t.ID() })
	})
	c.AddRepeatable("active_users_view", func(s *Schema) {
		s.Exec("CREATE OR REPLACE VIEW active_users AS SELECT * FROM users WHERE " + where)
	})
	return c
}

func repeatableRecord(t *testing.T, c *Collection, name string) record {
	t.Helper()
	mig := c.get(name)
	if mig == nil || !mig.repeatable {
		t.Fatalf("%q is not a registered repeatable migration", name)
	}
	sum, err := mig.checksum(Postgres)
	if err != nil {
		t.Fatal(err)
	}
	return record{version: name, batch: repeatableBatch, checksum: sum, appliedAt: "2026-07-08T00:00:00.000000Z"}
}

func TestRepeatableFirstRunExecutesAfterVersioned(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, viewCollection("active"))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	assertLogSequence(t, f.logged(), []string{
		`CREATE TABLE "users"`, // versioned first
		"INSERT INTO",
		"CREATE OR REPLACE VIEW", // repeatable after
		"INSERT INTO",
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	var batches []int64
	for _, args := range f.args {
		if len(args) == 4 { // record inserts
			batches = append(batches, args[1].Value.(int64))
		}
	}
	if len(batches) != 2 || batches[0] != 1 || batches[1] != repeatableBatch {
		t.Errorf("record batches = %v, want [1 -1]", batches)
	}
}

func TestRepeatableUnchangedSkips(t *testing.T) {
	f := newFakeDB()
	c := viewCollection("active")
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		repeatableRecord(t, c, "active_users_view"),
	)
	var buf bytes.Buffer
	m := testMigrator(t, f, Postgres, c, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(f.loggedContaining("CREATE OR REPLACE VIEW")) != 0 {
		t.Error("an unchanged repeatable migration must not re-run")
	}
	if !strings.Contains(buf.String(), "nothing to apply") {
		t.Error("expected nothing-to-apply")
	}
}

func TestRepeatableChangedRerunsAndUpdates(t *testing.T) {
	f := newFakeDB()
	old := viewCollection("active")
	edited := viewCollection("active AND verified") // definition changed
	f.setRecords(
		appliedRecord(t, edited, Postgres, "001_users", 1),
		repeatableRecord(t, old, "active_users_view"), // recorded checksum is stale
	)
	m := testMigrator(t, f, Postgres, edited)
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if len(f.loggedContaining("CREATE OR REPLACE VIEW")) != 1 {
		t.Fatal("the edited repeatable migration should re-run")
	}
	if len(f.loggedContaining("UPDATE \"schema_migrations\" SET checksum")) != 1 {
		t.Error("a re-run must update, not insert, its record")
	}
	if len(f.loggedContaining("INSERT INTO")) != 0 {
		t.Error("nothing should be inserted on a re-run")
	}
}

func TestRepeatableChecksumChangeIsNotDrift(t *testing.T) {
	f := newFakeDB()
	old := viewCollection("active")
	edited := viewCollection("banned")
	f.setRecords(
		appliedRecord(t, edited, Postgres, "001_users", 1),
		repeatableRecord(t, old, "active_users_view"),
	)
	// Strict checksum mode must not reject a pending repeatable re-run.
	m := testMigrator(t, f, Postgres, edited, WithStrictChecksum())
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up under strict checksums: %v", err)
	}
}

func TestRollbackNeverTouchesRepeatables(t *testing.T) {
	f := newFakeDB()
	c := viewCollection("active")
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		repeatableRecord(t, c, "active_users_view"),
	)
	m := testMigrator(t, f, Postgres, c)
	if err := m.RollbackBatch(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if len(f.loggedContaining(`DROP TABLE "users"`)) != 1 {
		t.Error("the versioned migration should roll back")
	}
	for _, entry := range f.loggedContaining("DELETE FROM") {
		if strings.Contains(entry, "batch") {
			t.Errorf("plain Rollback must not sweep repeatable records: %s", entry)
		}
	}
}

func TestResetForgetsRepeatableRecords(t *testing.T) {
	f := newFakeDB()
	c := viewCollection("active")
	f.setRecords(
		appliedRecord(t, c, Postgres, "001_users", 1),
		repeatableRecord(t, c, "active_users_view"),
	)
	m := testMigrator(t, f, Postgres, c)
	if err := m.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if len(f.loggedContaining("DELETE FROM \"schema_migrations\" WHERE batch")) != 1 {
		t.Error("Reset should forget repeatable records")
	}
}

func TestRepeatableStatusAndPlan(t *testing.T) {
	f := newFakeDB()
	old := viewCollection("active")
	edited := viewCollection("active AND verified")
	f.setRecords(
		appliedRecord(t, edited, Postgres, "001_users", 1),
		repeatableRecord(t, old, "active_users_view"),
	)
	m := testMigrator(t, f, Postgres, edited)

	sts, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	byName := map[string]Status{}
	for _, s := range sts {
		byName[s.Name] = s
	}
	view := byName["active_users_view"]
	if !view.Repeatable || !view.Applied || !view.Registered || !view.Drifted {
		t.Errorf("view status = %+v, want repeatable, applied, registered, drifted (pending re-run)", view)
	}
	if byName["001_users"].Repeatable {
		t.Error("versioned migrations must not be marked repeatable")
	}

	plans, err := m.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 1 || plans[0].Name != "active_users_view" {
		t.Fatalf("plan = %+v, want just the due repeatable", plans)
	}
}

func TestBaselineRecordsRepeatables(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, viewCollection("active"))
	if err := m.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if len(f.loggedContaining("CREATE OR REPLACE VIEW")) != 0 {
		t.Error("Baseline must not execute repeatable migrations")
	}
	if len(f.loggedContaining("INSERT INTO")) != 2 {
		t.Error("Baseline should record the versioned and the repeatable migration")
	}
}

func TestAddRepeatableWithDownPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	c := NewCollection()
	c.AddRepeatable("v", func(s *Schema) { s.Exec("SELECT 1") },
		WithDown(func(s *Schema) { s.Exec("SELECT 2") }))
}

func TestCollectionSQLIncludesRepeatables(t *testing.T) {
	plans, err := viewCollection("active").SQL(Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].Name != "001_users" || plans[1].Name != "active_users_view" {
		t.Fatalf("SQL should render versioned then repeatable, got %+v", plans)
	}
}
