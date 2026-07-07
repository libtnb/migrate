package migrate

import (
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
		`DROP TABLE "users"`,
		`ALTER TABLE "users__migrate_new" RENAME TO "users"`,
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email")`,
	}
	assertSQL(t, got, want)
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
		`CONSTRAINT "orders_pkey" PRIMARY KEY ("code")`,       // not orders__migrate_new_pkey
		`CONSTRAINT "orders_state_check" CHECK ("state" IN (`, // not orders__migrate_new_state_check
	} {
		if !strings.Contains(create, frag) {
			t.Errorf("missing %q in:\n%s", frag, create)
		}
	}
	if strings.Contains(create, "__migrate_new_") {
		t.Errorf("no constraint may carry the temporary name:\n%s", create)
	}
}

func TestRecreateMySQLUsesRenameTable(t *testing.T) {
	got := compileSchema(t, MySQL, func(s *Schema) {
		s.Recreate("t", func(t *Table) { t.ID() })
	})
	found := false
	for _, sql := range got {
		if sql == "RENAME TABLE `t__migrate_new` TO `t`" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected RENAME TABLE statement, got:\n%s", strings.Join(got, "\n"))
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
