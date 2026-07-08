package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// DB is the querying surface handed to Run functions. Both *sql.Tx and
// *sql.DB satisfy it, so data migrations read the same whether the migration
// runs inside a transaction (the default) or outside one (WithoutTransaction).
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

var (
	_ DB = (*sql.Tx)(nil)
	_ DB = (*sql.DB)(nil)
)

// Schema records the operations a migration declares. A migration function
// receives a fresh Schema, declares changes with its methods, and returns;
// nothing touches the database while declaring. The recorded operations are
// then compiled to dialect SQL to apply the migration, reversed to roll it
// back, rendered for dry runs, and hashed into the migration's checksum.
//
// Because the function is re-run each time the migration is applied, rolled
// back or planned, it must be deterministic: derive nothing from the clock,
// randomness or external state.
type Schema struct {
	ops  []operation
	errs []error
}

func (s *Schema) record(op operation) {
	s.ops = append(s.ops, op)
}

func (s *Schema) errf(format string, a ...any) {
	s.errs = append(s.errs, fmt.Errorf(format, a...))
}

func (s *Schema) requireTable(method, table string) {
	if table == "" {
		s.errf("%s declares an empty table name", method)
	}
}

// Create declares a new table. The function receives a Table on which columns,
// indexes and foreign keys are declared:
//
//	s.Create("users", func(t *migrate.Table) {
//		t.ID()
//		t.String("email").Unique()
//		t.Timestamps()
//	})
//
// Rolling back a Create drops the table.
func (s *Schema) Create(table string, fn func(*Table)) {
	s.requireTable("Create", table)
	def := &tableDef{name: table}
	t := &Table{table: table, create: def}
	if fn != nil {
		fn(t)
	}
	if len(def.columns) == 0 {
		s.errf("Create(%q) declares no columns", table)
	}
	s.record(&createTable{def: def})
}

// Table declares changes to an existing table: adding or dropping columns,
// indexes and foreign keys, or renaming columns. Each change compiles to its
// own statement; rolling back reverses the changes in reverse order.
func (s *Schema) Table(table string, fn func(*Table)) {
	s.requireTable("Table", table)
	alter := &alterTable{table: table}
	t := &Table{table: table, alter: alter}
	if fn != nil {
		fn(t)
	}
	if len(alter.changes) == 0 {
		s.errf("Table(%q) declares no changes", table)
	}
	s.record(alter)
}

// Recreate replaces a table with a new definition while preserving its rows,
// the portable way to change what ALTER TABLE cannot — on SQLite that is any
// constraint change. The function declares the complete target table, exactly
// like Create; every declared column is copied from the old table by name,
// except those marked SkipCopy, which start from their default:
//
//	s.Recreate("users", func(t *migrate.Table) {
//		t.ID()
//		t.String("email").Unique()
//		t.ForeignID("team_id").Constrained().Nullable().SkipCopy() // new column
//	})
//
// It compiles to: create a temporary table with the new shape, copy the rows,
// drop the old table, rename into place, rebuild indexes. On Postgres and
// SQLite the whole sequence runs inside the migration's transaction, so a
// failure leaves the original table untouched. MySQL refuses to compile a
// Recreate: its implicit DDL commits would leave a crash window with the
// live table dropped, and native ALTER TABLE already covers every Recreate
// use case there. Child tables referencing the recreated one keep working —
// their foreign keys resolve by name once the rename lands — but with SQLite
// foreign key enforcement enabled (PRAGMA foreign_keys=ON) and referencing
// rows present, run the migration on a connection with enforcement off.
//
// Recreate discards the previous definition and is therefore irreversible;
// rolling back requires WithDown.
func (s *Schema) Recreate(table string, fn func(*Table)) {
	s.requireTable("Recreate", table)
	def := &tableDef{name: table}
	t := &Table{table: table, create: def}
	if fn != nil {
		fn(t)
	}
	if len(def.columns) == 0 {
		s.errf("Recreate(%q) declares no columns", table)
	}
	s.record(&recreateTable{def: def})
}

// Rename renames a table. It reverses to the opposite rename.
func (s *Schema) Rename(from, to string) {
	s.requireTable("Rename", from)
	s.requireTable("Rename", to)
	s.record(&renameTable{from: from, to: to})
}

// Drop removes a table. Dropping is irreversible: rolling back requires an
// explicit down migration declared with WithDown.
func (s *Schema) Drop(table string) {
	s.requireTable("Drop", table)
	s.record(&dropTable{name: table})
}

// DropIfExists removes a table if it exists. Like Drop, it is irreversible.
func (s *Schema) DropIfExists(table string) {
	s.requireTable("DropIfExists", table)
	s.record(&dropTable{name: table, ifExists: true})
}

// Exec declares a raw SQL statement for whatever the schema builder does not
// cover. The statement participates in dry runs and the checksum, but cannot
// be automatically reversed; rolling back requires WithDown.
//
// Args use the dialect's native placeholders ($1 for Postgres, ? otherwise).
func (s *Schema) Exec(query string, args ...any) {
	if strings.TrimSpace(query) == "" {
		s.errf("Exec declares an empty SQL statement")
	}
	s.record(&rawSQL{sql: query, args: args})
}

// Run declares a Go function, for data migrations that need queries or
// application logic. The function runs inside the migration's transaction
// when there is one and must not retain db after returning.
//
// A Go function is opaque: dry runs list it without SQL, the checksum does
// not cover its body, and it cannot be automatically reversed.
func (s *Schema) Run(fn func(ctx context.Context, db DB) error) {
	if fn == nil {
		s.errf("Run declares a nil function")
		return
	}
	s.record(&goFunc{fn: fn})
}
