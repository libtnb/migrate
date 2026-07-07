package migrate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestAnalyzeSafetyFindings(t *testing.T) {
	cases := map[string]struct {
		dialect string
		declare func(*Schema)
		want    string // empty means no findings expected
	}{
		"drop table": {
			"postgres", func(s *Schema) { s.Drop("users") }, "dropping table",
		},
		"rename table": {
			"postgres", func(s *Schema) { s.Rename("a", "b") }, "not backward compatible",
		},
		"not null without default": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.String("c") }) },
			"fails when rows exist",
		},
		"drop column": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.DropColumn("c") }) },
			"deploy code that stopped using it",
		},
		"rename column": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.RenameColumn("a", "b") }) },
			"dual-writing",
		},
		"index on postgres": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.Index("c") }) },
			"CREATE INDEX CONCURRENTLY",
		},
		"foreign key on postgres": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.Foreign("c").References("p") }) },
			"NOT VALID",
		},
		"add primary": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.Primary("a", "b") }) },
			"rewrites the table",
		},
		// Safe declarations produce no findings.
		"create table is safe": {
			"postgres",
			func(s *Schema) {
				s.Create("t", func(t *Table) {
					t.ID()
					t.String("c").Index()
					t.ForeignID("p_id").Constrained()
				})
			},
			"",
		},
		"nullable add is safe": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.String("c").Nullable() }) },
			"",
		},
		"defaulted add is safe": {
			"postgres",
			func(s *Schema) { s.Table("t", func(t *Table) { t.String("c").Default("x") }) },
			"",
		},
		"index off postgres is quiet": {
			"mysql",
			func(s *Schema) { s.Table("t", func(t *Table) { t.Index("c") }) },
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := &Schema{}
			tc.declare(s)
			findings := analyzeSafety(tc.dialect, s.ops)
			if tc.want == "" {
				if len(findings) != 0 {
					t.Fatalf("expected no findings, got %v", findings)
				}
				return
			}
			if len(findings) == 0 || !strings.Contains(strings.Join(findings, "\n"), tc.want) {
				t.Fatalf("findings %v should mention %q", findings, tc.want)
			}
		})
	}
}

func dangerousCollection() *Collection {
	c := NewCollection()
	c.Add("001_users", func(s *Schema) {
		s.Create("users", func(t *Table) { t.ID() })
	})
	c.Add("002_drop_legacy", func(s *Schema) {
		s.Table("users", func(t *Table) { t.DropColumn("legacy") })
	}, WithDown(func(s *Schema) {}))
	return c
}

func TestSafetyWarnsByDefaultAndProceeds(t *testing.T) {
	f := newFakeDB()
	var buf bytes.Buffer
	m := testMigrator(t, f, Postgres, dangerousCollection(),
		WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up should proceed under SafetyWarn: %v", err)
	}
	if !strings.Contains(buf.String(), "safety") || !strings.Contains(buf.String(), "002_drop_legacy") {
		t.Errorf("expected a safety warning naming the migration, log:\n%s", buf.String())
	}
	if len(f.loggedContaining("DROP COLUMN")) != 1 {
		t.Error("the migration should still run")
	}
}

func TestSafetyStrictRefusesBeforeExecuting(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, dangerousCollection(), WithSafety(SafetyStrict))
	err := m.Up(context.Background())
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("err = %v, want ErrUnsafe", err)
	}
	for _, frag := range []string{"002_drop_legacy", "Assured()"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("error should mention %q, got: %v", frag, err)
		}
	}
	// Nothing may have executed — not even the safe first migration.
	if len(f.loggedContaining("CREATE TABLE \"users\"")) != 0 || len(f.loggedContaining("INSERT INTO")) != 0 {
		t.Error("SafetyStrict must refuse before executing anything")
	}
}

func TestSafetyAssuredSkipsAnalysis(t *testing.T) {
	f := newFakeDB()
	c := NewCollection()
	c.Add("001_users", func(s *Schema) {
		s.Create("users", func(t *Table) { t.ID() })
	})
	c.Add("002_drop_legacy", func(s *Schema) {
		s.Table("users", func(t *Table) { t.DropColumn("legacy") })
	}, WithDown(func(s *Schema) {}), Assured())
	m := testMigrator(t, f, Postgres, c, WithSafety(SafetyStrict))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("an Assured migration must pass strict safety: %v", err)
	}
}

func TestSafetyOffIsSilent(t *testing.T) {
	f := newFakeDB()
	var buf bytes.Buffer
	m := testMigrator(t, f, Postgres, dangerousCollection(),
		WithSafety(SafetyOff), WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if strings.Contains(buf.String(), "safety") {
		t.Error("SafetyOff must not log findings")
	}
}

func TestPlanCarriesWarnings(t *testing.T) {
	f := newFakeDB()
	m := testMigrator(t, f, Postgres, dangerousCollection())
	plans, err := m.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	if len(plans[0].Warnings) != 0 {
		t.Errorf("creating a table should carry no warnings, got %v", plans[0].Warnings)
	}
	if len(plans[1].Warnings) == 0 || !strings.Contains(plans[1].Warnings[0], "dropping column") {
		t.Errorf("the drop should carry a warning, got %v", plans[1].Warnings)
	}
}
