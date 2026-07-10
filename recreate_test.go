package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRecreateCompilesMoveAndCopy(t *testing.T) {
	got := compileSchema(t, SQLite, func(s *Schema) {
		s.Recreate("users", func(t *Table) {
			t.ID()
			t.String("email").Unique()
			t.ForeignID("team_id").Constrained().Nullable().SkipCopy()
		})
	})
	want := []string{
		`CREATE TABLE "users__migrate_new" (
	"id" INTEGER PRIMARY KEY AUTOINCREMENT,
	"email" VARCHAR(255) NOT NULL,
	"team_id" INTEGER,
	CONSTRAINT "users_team_id_foreign" FOREIGN KEY ("team_id") REFERENCES "teams" ("id")
)`,
		`INSERT INTO "users__migrate_new" ("id", "email") SELECT "id", "email" FROM "users"`,
		`-- capture the triggers of "users"`,
		`DROP TABLE "users"`,
		`ALTER TABLE "users__migrate_new" RENAME TO "users"`,
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email")`,
		`-- recreate the captured triggers of "users"`,
	}
	assertSQL(t, got, want)
}

// Audit H5: DROP TABLE takes the table's triggers with it, so the sequence
// must capture their DDL before the drop and replay it once the rename
// restores the original name — the order the SQLite twelve-step ALTER TABLE
// procedure prescribes.
func TestRecreateReplaysCapturedTriggers(t *testing.T) {
	for name, tc := range map[string]struct {
		d       Dialect
		capture string // fragment of the trigger-capture query
	}{
		"sqlite":   {SQLite, "type = 'trigger'"},
		"postgres": {Postgres, "pg_trigger"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDB()
			f.triggers = []string{"CREATE TRIGGER users_audit AFTER INSERT ON users BEGIN INSERT INTO audit (email) VALUES (new.email); END"}
			c := NewCollection()
			c.Add("001_rebuild", func(s *Schema) {
				s.Recreate("users", func(t *Table) { t.ID() })
			}, WithDown(func(s *Schema) {}))
			m := testMigrator(t, f, tc.d, c)
			if err := m.Up(context.Background()); err != nil {
				t.Fatalf("Up: %v", err)
			}
			assertLogSequence(t, f.logged(), []string{
				tc.capture, // capture runs before the drop
				`DROP TABLE "users"`,
				`RENAME TO "users"`,
				"CREATE TRIGGER users_audit", // replay lands after the rename
			})
			// The bookkeeping row is written in the same transaction; where
			// in it is dialect business (SQLite records first, see LB11).
			if len(f.loggedContaining(`INSERT INTO "schema_migrations"`)) != 1 {
				t.Error("the migration must be recorded")
			}
		})
	}
}

func TestRecreateConstraintNamesUseFinalTable(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Recreate("orders", func(t *Table) {
			t.Integer("code").Primary()
			t.Enum("state", "open", "closed")
		})
	})
	create := got[0]
	for _, frag := range []string{
		`CREATE TABLE "orders__migrate_new"`,
		// The PK stays unnamed here: a named one would create a backing index
		// colliding with the live table's orders_pkey. It is renamed to the
		// conventional name after the swap.
		`PRIMARY KEY ("code")`,
		`CONSTRAINT "orders_state_check" CHECK ("state" IN (`, // not orders__migrate_new_state_check
	} {
		if !strings.Contains(create, frag) {
			t.Errorf("missing %q in:\n%s", frag, create)
		}
	}
	if strings.Contains(create, `CONSTRAINT "orders_pkey"`) {
		t.Errorf("the temp table must not claim the live pkey name:\n%s", create)
	}
	last := got[len(got)-1]
	if last != `ALTER TABLE "orders" RENAME CONSTRAINT "orders__migrate_new_pkey" TO "orders_pkey"` {
		t.Errorf("the swap should finish by renaming the PK constraint, got: %s", last)
	}
}

func TestRecreateIdentitySequenceAdvances(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Recreate("users", func(t *Table) {
			t.ID()
			t.String("email")
		})
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, `RENAME CONSTRAINT "users__migrate_new_pkey" TO "users_pkey"`) {
		t.Errorf("identity PK should be renamed after the swap:\n%s", joined)
	}
	want := `SELECT setval(pg_get_serial_sequence('"users"', 'id'), COALESCE((SELECT MAX("id") FROM "users"), 0) + 1, false)`
	if got[len(got)-1] != want {
		t.Errorf("the identity sequence must advance past copied rows:\n got: %s\nwant: %s", got[len(got)-1], want)
	}

	// A skipped-copy identity column keeps its fresh sequence: no setval.
	got = compileSchema(t, Postgres, func(s *Schema) {
		s.Recreate("users", func(t *Table) {
			t.ID().SkipCopy()
			t.String("email")
		})
	})
	if strings.Contains(strings.Join(got, "\n"), "setval") {
		t.Error("a SkipCopy identity column needs no sequence bump")
	}
}

// Codex adversarial review: MySQL's implicit DDL commits leave a crash window
// between the DROP and the RENAME with the live table gone; native ALTER
// covers every Recreate use case there, so compiling is refused outright.
func TestMySQLRefusesRecreate(t *testing.T) {
	s := &Schema{}
	s.Recreate("t", func(t *Table) { t.ID() })
	_, err := MySQL.compile(s.ops[0])
	if err == nil || !strings.Contains(err.Error(), "ALTER") {
		t.Fatalf("MySQL must refuse Recreate and point at native ALTER, got: %v", err)
	}
}

func TestRecreateIsIrreversible(t *testing.T) {
	m := migrationOf(t, func(s *Schema) {
		s.Recreate("users", func(t *Table) { t.ID() })
	})
	if _, err := m.downOps(); !errors.Is(err, ErrIrreversible) {
		t.Fatalf("err = %v, want ErrIrreversible", err)
	}
}

func TestRecreateDeclarationValidation(t *testing.T) {
	cases := map[string]func(*Schema){
		"no columns": func(s *Schema) { s.Recreate("t", nil) },
		"empty name": func(s *Schema) { s.Recreate("", func(t *Table) { t.ID() }) },
		"bad auto-increment": func(s *Schema) {
			s.Recreate("t", func(t *Table) { t.String("id").AutoIncrement() })
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

func TestRecreateSafetyFinding(t *testing.T) {
	s := &Schema{}
	s.Recreate("users", func(t *Table) { t.ID() })
	findings := analyzeSafety("sqlite", s.ops)
	if len(findings) != 1 || !strings.Contains(findings[0], "copies every row") {
		t.Fatalf("expected a recreate finding, got %v", findings)
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
