package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func migrationOf(t *testing.T, up func(*Schema), opts ...MigrationOption) *Migration {
	t.Helper()
	c := NewCollection()
	c.Add("m", up, opts...)
	return c.get("m")
}

// downSQL compiles the migration's rollback for the dialect.
func downSQL(t *testing.T, d Dialect, m *Migration) []string {
	t.Helper()
	stmts, err := m.compile(d, false)
	if err != nil {
		t.Fatalf("compile down: %v", err)
	}
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = s.sql
	}
	return out
}

func TestAutomaticDownReversesInOrder(t *testing.T) {
	m := migrationOf(t, func(s *Schema) {
		s.Create("teams", func(t *Table) { t.ID() })
		s.Create("users", func(t *Table) {
			t.ID()
			t.ForeignID("team_id").Constrained()
		})
		s.Rename("users", "members")
	})
	got := downSQL(t, Postgres, m)
	want := []string{
		`ALTER TABLE "members" RENAME TO "users"`,
		`DROP TABLE "users"`,
		`DROP TABLE "teams"`,
	}
	assertSQL(t, got, want)
}

func TestAutomaticDownForAlter(t *testing.T) {
	m := migrationOf(t, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.String("nickname").Nullable().Index()
			t.RenameColumn("name", "full_name")
			t.Unique("email")
			t.Foreign("org_id").References("orgs")
		})
	})
	got := downSQL(t, Postgres, m)
	// Changes reverse in reverse order; the added column drops last.
	want := []string{
		`ALTER TABLE "users" DROP CONSTRAINT "users_org_id_foreign"`,
		`DROP INDEX "users_email_unique"`,
		`ALTER TABLE "users" RENAME COLUMN "full_name" TO "name"`,
		`DROP INDEX "users_nickname_index"`,
		`ALTER TABLE "users" DROP COLUMN "nickname"`,
	}
	assertSQL(t, got, want)
}

func TestIrreversibleOperationsRefuseToReverse(t *testing.T) {
	cases := map[string]func(*Schema){
		"drop table":  func(s *Schema) { s.Drop("users") },
		"drop column": func(s *Schema) { s.Table("users", func(t *Table) { t.DropColumn("bio") }) },
		"drop index":  func(s *Schema) { s.Table("users", func(t *Table) { t.DropIndex("email") }) },
		"raw sql":     func(s *Schema) { s.Exec("UPDATE users SET x = 1") },
		"go func":     func(s *Schema) { s.Run(func(context.Context, DB) error { return nil }) },
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			m := migrationOf(t, up)
			_, err := m.downOps()
			if !errors.Is(err, ErrIrreversible) {
				t.Fatalf("err = %v, want ErrIrreversible", err)
			}
		})
	}
}

func TestExplicitDownWins(t *testing.T) {
	m := migrationOf(t,
		func(s *Schema) { s.Exec("CREATE VIEW v AS SELECT 1") },
		WithDown(func(s *Schema) { s.Exec("DROP VIEW v") }),
	)
	got := downSQL(t, Postgres, m)
	assertSQL(t, got, []string{"DROP VIEW v"})
}

func TestDeclarationErrorsSurfaceOnCompile(t *testing.T) {
	cases := map[string]func(*Schema){
		"empty column name":      func(s *Schema) { s.Create("t", func(t *Table) { t.String("") }) },
		"enum without values":    func(s *Schema) { s.Create("t", func(t *Table) { t.Enum("status") }) },
		"empty raw type":         func(s *Schema) { s.Create("t", func(t *Table) { t.Column("c", "") }) },
		"index without columns":  func(s *Schema) { s.Create("t", func(t *Table) { t.Index() }) },
		"alter-only in create":   func(s *Schema) { s.Create("t", func(t *Table) { t.DropColumn("x") }) },
		"unconstrained cascade":  func(s *Schema) { s.Create("t", func(t *Table) { t.ForeignID("u_id").CascadeOnDelete() }) },
		"unguessable parent":     func(s *Schema) { s.Create("t", func(t *Table) { t.ForeignID("owner").Constrained() }) },
		"empty table name":       func(s *Schema) { s.Create("", func(t *Table) { t.ID() }) },
		"create without columns": func(s *Schema) { s.Create("t", nil) },
		"alter without changes":  func(s *Schema) { s.Table("t", nil) },
		"empty exec":             func(s *Schema) { s.Exec("  ") },
		"nil run":                func(s *Schema) { s.Run(nil) },
		"empty drop":             func(s *Schema) { s.Drop("") },
		"empty rename":           func(s *Schema) { s.Rename("a", "") },
	}
	for name, up := range cases {
		t.Run(name, func(t *testing.T) {
			m := migrationOf(t, up)
			if _, err := m.upOps(); err == nil {
				t.Fatal("expected a declaration error, got none")
			}
		})
	}
}

func TestChecksumStableAndSensitive(t *testing.T) {
	build := func(email string) *Migration {
		c := NewCollection()
		c.Add("m", func(s *Schema) {
			s.Create("users", func(t *Table) {
				t.ID()
				t.String(email).Unique()
			})
		})
		return c.get("m")
	}
	a1, err := build("email").checksum(Postgres)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := build("email").checksum(Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Error("checksum is not deterministic")
	}
	b, err := build("mail").checksum(Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if a1 == b {
		t.Error("checksum ignores declaration changes")
	}
	lite, err := build("email").checksum(SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if a1 == lite {
		t.Error("checksum should be dialect specific")
	}
}

func TestAddPanics(t *testing.T) {
	cases := map[string]func(*Collection){
		"empty name": func(c *Collection) { c.Add("", func(*Schema) {}) },
		"nil up":     func(c *Collection) { c.Add("m", nil) },
		"whitespace": func(c *Collection) { c.Add(" m", func(*Schema) {}) },
		"too long":   func(c *Collection) { c.Add(strings.Repeat("x", 192), func(*Schema) {}) },
		"duplicate":  func(c *Collection) { c.Add("m", func(*Schema) {}); c.Add("m", func(*Schema) {}) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			fn(NewCollection())
		})
	}
}

func TestCollectionSortsByName(t *testing.T) {
	c := NewCollection()
	c.Add("20260102000000_b", func(*Schema) {})
	c.Add("20260101000000_a", func(*Schema) {})
	c.Add("20260103000000_c", func(*Schema) {})
	got := c.sorted()
	names := make([]string, len(got))
	for i, m := range got {
		names[i] = m.Name()
	}
	want := []string{"20260101000000_a", "20260102000000_b", "20260103000000_c"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("order = %v, want %v", names, want)
		}
	}
}

func TestGuessParentTable(t *testing.T) {
	cases := map[string]string{
		"user_id":     "users",
		"category_id": "categories",
		"box_id":      "boxes",
		"branch_id":   "branches",
		"day_id":      "days",
		"team_id":     "teams",
		"owner":       "", // no _id suffix
		"_id":         "",
	}
	for col, want := range cases {
		if got := guessParentTable(col); got != want {
			t.Errorf("guessParentTable(%q) = %q, want %q", col, got, want)
		}
	}
}
