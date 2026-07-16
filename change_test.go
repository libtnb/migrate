package migrate

import (
	"errors"
	"testing"
)

// Change restates the complete target definition; MySQL compiles it to one
// MODIFY COLUMN carrying every clause.
func TestChangeColumnMySQL(t *testing.T) {
	got := compileSchema(t, MySQL, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.String("name", 500).Nullable().Default("unknown").Comment("display name").Change()
		})
	})
	assertSQL(t, got, []string{
		"ALTER TABLE `users` MODIFY COLUMN `name` VARCHAR(500) DEFAULT 'unknown' COMMENT 'display name'",
	})
}

// Postgres has no MODIFY: the type, nullability and default each change on
// their own, and an omitted default is dropped — a restatement, not a patch.
func TestChangeColumnPostgres(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.String("name", 500).Default("unknown").Change()
		})
	})
	assertSQL(t, got, []string{
		`ALTER TABLE "users" ALTER COLUMN "name" TYPE VARCHAR(500)`,
		`ALTER TABLE "users" ALTER COLUMN "name" SET NOT NULL`,
		`ALTER TABLE "users" ALTER COLUMN "name" SET DEFAULT 'unknown'`,
	})

	nullable := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.Integer("age").Nullable().Change().Using("age::integer")
		})
	})
	assertSQL(t, nullable, []string{
		`ALTER TABLE "users" ALTER COLUMN "age" TYPE INTEGER USING age::integer`,
		`ALTER TABLE "users" ALTER COLUMN "age" DROP NOT NULL`,
		`ALTER TABLE "users" ALTER COLUMN "age" DROP DEFAULT`,
	})
}

func TestChangeColumnSQLiteRefused(t *testing.T) {
	err := compileErr(SQLite, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name", 500).Change() })
	})
	assertErrContains(t, err, "Schema.Recreate")
}

func TestChangeColumnDeclarationRules(t *testing.T) {
	// Change outside Schema.Table is meaningless.
	err := compileErr(SQLite, func(s *Schema) {
		s.Create("users", func(t *Table) {
			t.ID()
			t.String("name").Change()
		})
	})
	assertErrContains(t, err, "only valid inside Schema.Table")

	// Using without Change converts nothing.
	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name").Using("name::text") })
	})
	assertErrContains(t, err, "Using without Change")

	// Index modifiers cannot be restated: the existing indexes stay.
	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name").Unique().Change() })
	})
	assertErrContains(t, err, "Unique/Index")

	// Using is the Postgres conversion expression; MySQL converts implicitly.
	err = compileErr(MySQL, func(s *Schema) {
		s.Table("users", func(t *Table) { t.Integer("age").Change().Using("age::integer") })
	})
	assertErrContains(t, err, "implicitly")

	// Postgres emulates enums with a check constraint Change cannot restate.
	err = compileErr(Postgres, func(s *Schema) {
		s.Table("users", func(t *Table) { t.Enum("role", "a", "b").Change() })
	})
	assertErrContains(t, err, "check constraint")
}

// A changed column discards its previous definition, so the automatic down
// refuses and asks for WithDown.
func TestChangeColumnIrreversible(t *testing.T) {
	m := &Migration{name: "m", useTx: true, up: func(s *Schema) {
		s.Table("users", func(t *Table) { t.String("name", 500).Change() })
	}}
	if _, err := m.downOps(); !errors.Is(err, ErrIrreversible) {
		t.Fatalf("downOps error = %v, want ErrIrreversible", err)
	}
}
