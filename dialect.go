package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Dialect generates SQL for and drives one database engine. The built-in
// dialects are Postgres, MySQL and SQLite; the interface is opaque so the
// grammar can evolve without breaking third parties.
type Dialect interface {
	name() string

	// compile turns one recorded operation into executable statements.
	compile(op operation) ([]statement, error)

	// ensureTableSQL creates the migration records table if missing.
	ensureTableSQL(table string) string
	// quoteIdent escapes an identifier for the dialect.
	quoteIdent(name string) string
	// placeholder renders the n-th (1-based) query placeholder.
	placeholder(n int) string
	// transactionalDDL reports whether DDL statements roll back with the
	// surrounding transaction.
	transactionalDDL() bool

	// lock takes a database-wide advisory lock named for the records table,
	// blocking up to timeout, and unlock releases it. Both run on the same
	// dedicated connection.
	lock(ctx context.Context, conn *sql.Conn, table string, timeout time.Duration) error
	unlock(ctx context.Context, conn *sql.Conn, table string) error

	// listTablesSQL queries the names of every base table in the current
	// schema, one string column.
	listTablesSQL() string
	// freshDropSQL drops one table during Fresh, cascading where the engine
	// supports it.
	freshDropSQL(table string) string
}

// statement is one executable unit of a compiled migration: either SQL text
// or an opaque Go function.
type statement struct {
	sql  string
	args []any
	fn   func(context.Context, DB) error
}

func sqlStatement(format string, a ...any) statement {
	return statement{sql: fmt.Sprintf(format, a...)}
}

// quoter escapes identifiers with the dialect's quote character.
type quoter byte

func (q quoter) ident(name string) string {
	c := string(q)
	return c + strings.ReplaceAll(name, c, c+c) + c
}

func (q quoter) idents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = q.ident(n)
	}
	return strings.Join(quoted, ", ")
}

// table quotes a possibly schema-qualified table name: dots separate path
// segments, each quoted on its own ("analytics.events" → "analytics"."events").
// Table names therefore cannot contain literal dots — the standard trade-off
// every schema-aware tool makes.
func (q quoter) table(name string) string {
	segs := strings.Split(name, ".")
	for i, s := range segs {
		segs[i] = q.ident(s)
	}
	return strings.Join(segs, ".")
}

// baseName strips the schema qualification from a table name. Conventional
// constraint and index names build on it: constraints live inside the table's
// schema already, and Postgres refuses qualified names in those positions.
func baseName(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[i+1:]
	}
	return table
}

// schemaPrefix returns the qualification up to and including the final dot,
// or "" for a bare name. Postgres and SQLite drop indexes by qualified name.
func schemaPrefix(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[:i+1]
	}
	return ""
}

// literal renders a Go value as a SQL literal for DDL default clauses, which
// cannot use bind parameters. backslashEscapes marks dialects (MySQL) whose
// strings treat backslash as an escape character.
func literal(v any, backslashEscapes bool) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		s := strings.ReplaceAll(x, "'", "''")
		if backslashEscapes {
			s = strings.ReplaceAll(s, `\`, `\\`)
		}
		return "'" + s + "'", nil
	case bool:
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", x), nil
	case float32, float64:
		return fmt.Sprintf("%v", x), nil
	default:
		return "", fmt.Errorf("migrate: unsupported default value of type %T; use DefaultExpr for SQL expressions", v)
	}
}

// enumCheckSQL renders the inline column CHECK constraint that emulates an
// enum on dialects without a native type. Attaching it to the column keeps
// CREATE TABLE and ADD COLUMN on the same path (SQLite cannot add table
// constraints after the fact).
func enumCheckSQL(q quoter, table string, c *columnDef) string {
	vals := make([]string, len(c.enumVals))
	for i, v := range c.enumVals {
		vals[i] = "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return fmt.Sprintf(" CONSTRAINT %s CHECK (%s IN (%s))",
		q.ident(baseName(table)+"_"+c.name+"_check"), q.ident(c.name), strings.Join(vals, ", "))
}

// generatedClause renders GENERATED ALWAYS AS (...) STORED|VIRTUAL, shared
// verbatim by all three dialects.
func generatedClause(c *columnDef) string {
	if c.generatedExpr == "" {
		return ""
	}
	kind := " STORED"
	if c.generatedVirtual {
		kind = " VIRTUAL"
	}
	return " GENERATED ALWAYS AS (" + c.generatedExpr + ")" + kind
}

// checkClause renders a named table-level CHECK constraint.
func checkClause(q quoter, chk *checkDef) string {
	return fmt.Sprintf("CONSTRAINT %s CHECK (%s)", q.ident(chk.name), chk.expr)
}

// defaultClause renders the DEFAULT part of a column definition. currentTS is
// the dialect's current-timestamp expression, which MySQL requires to carry
// the column's fractional precision.
func defaultClause(c *columnDef, backslashEscapes bool, currentTS string) (string, error) {
	switch {
	case c.useCurrent:
		return " DEFAULT " + currentTS, nil
	case c.defaultExpr != "":
		return " DEFAULT (" + c.defaultExpr + ")", nil
	case c.hasDefault:
		lit, err := literal(c.defaultVal, backslashEscapes)
		if err != nil {
			return "", fmt.Errorf("column %q: %w", c.name, err)
		}
		return " DEFAULT " + lit, nil
	}
	return "", nil
}

// foreignClause renders an inline FOREIGN KEY table constraint, shared by all
// dialects.
func foreignClause(q quoter, table string, fk *foreignDef) string {
	refCols := fk.refColumns
	if len(refCols) == 0 {
		refCols = []string{"id"}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		q.ident(fk.resolvedName(table)), q.idents(fk.columns), q.table(fk.refTable), q.idents(refCols))
	if fk.onDelete != "" {
		b.WriteString(" ON DELETE " + fk.onDelete)
	}
	if fk.onUpdate != "" {
		b.WriteString(" ON UPDATE " + fk.onUpdate)
	}
	return b.String()
}

// createIndexSQL renders a standalone CREATE INDEX, shared by all dialects.
// Single-column indexes declared with Column.Unique/Index compile through
// here too, so every index carries the conventional, reconstructible name.
//
// Engines disagree on where a schema qualification goes: SQLite attaches it
// to the index name (the table must be bare), Postgres and MySQL to the table
// (the index name must be bare).
func createIndexSQL(q quoter, table string, idx *indexDef, schemaOnIndex bool) string {
	unique := ""
	if idx.unique {
		unique = "UNIQUE "
	}
	name := idx.resolvedName(table)
	if schemaOnIndex {
		return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)",
			unique, q.table(schemaPrefix(table)+name), q.ident(baseName(table)), q.idents(idx.columns))
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)",
		unique, q.ident(name), q.table(table), q.idents(idx.columns))
}

// charLength defends against zero and negative declared lengths; the fluent
// declarations already default to 255.
func charLength(n int) int {
	if n <= 0 {
		return 255
	}
	return n
}

// inlineIndexes collects the index definitions implied by column modifiers so
// they compile as standalone statements alongside explicitly declared ones.
func inlineIndexes(cols []*columnDef) []*indexDef {
	var idxs []*indexDef
	for _, c := range cols {
		if c.unique {
			idxs = append(idxs, &indexDef{columns: []string{c.name}, unique: true})
		}
		if c.indexed {
			idxs = append(idxs, &indexDef{columns: []string{c.name}})
		}
	}
	return idxs
}

// primaryColumns resolves the table-level primary key columns, validating
// that an auto-incrementing column (whose PRIMARY KEY is rendered inline) is
// not combined with other primary key declarations.
func primaryColumns(def *tableDef) ([]string, error) {
	var inline bool
	var cols []string
	for _, c := range def.columns {
		if c.inlinePrimary() {
			if inline {
				return nil, fmt.Errorf("migrate: table %q declares more than one auto-incrementing column", def.name)
			}
			inline = true
			continue
		}
		if c.primary {
			cols = append(cols, c.name)
		}
	}
	cols = append(cols, def.primary...)
	if inline && len(cols) > 0 {
		return nil, fmt.Errorf("migrate: table %q combines an auto-incrementing primary key with other primary key declarations", def.name)
	}
	return cols, nil
}

func declarationErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("migrate: invalid declaration: %w", errors.Join(errs...))
}

// compileRecreate builds the move-and-copy sequence shared by every dialect:
// create a temporary table with the target shape, copy the surviving rows,
// drop the old table, rename into place, rebuild indexes. Constraints on the
// temporary table are pinned to their conventional final names via
// constraintBase, so nothing keeps a temporary name after the rename. Child
// foreign keys referencing the table resolve by name again once the rename
// lands — the same order Alembic uses for SQLite batch mode.
func compileRecreate(d Dialect, q quoter, schemaOnIndex bool, renameSQL func(from, to string) statement, def *tableDef) ([]statement, error) {
	tmp := def.name + "__migrate_new"
	tmpDef := &tableDef{
		name:           tmp,
		constraintBase: def.name,
		primary:        def.primary,
		checks:         def.checks,
		comment:        def.comment,
	}
	for _, c := range def.columns {
		cc := *c
		cc.unique, cc.indexed = false, false // indexes rebuild after the rename
		tmpDef.columns = append(tmpDef.columns, &cc)
	}
	for _, fk := range def.fks {
		f := *fk
		f.name = fk.resolvedName(def.name)
		tmpDef.fks = append(tmpDef.fks, &f)
	}

	stmts, err := d.compile(&createTable{def: tmpDef})
	if err != nil {
		return nil, err
	}
	var copyCols, copyExprs []string
	for _, c := range def.columns {
		if c.skipCopy || c.generatedExpr != "" { // generated columns fill themselves
			continue
		}
		copyCols = append(copyCols, c.name)
		if c.copyFrom != "" {
			copyExprs = append(copyExprs, c.copyFrom)
		} else {
			copyExprs = append(copyExprs, q.ident(c.name))
		}
	}
	if len(copyCols) > 0 {
		stmts = append(stmts, sqlStatement("INSERT INTO %s (%s) SELECT %s FROM %s",
			q.table(tmp), q.idents(copyCols), strings.Join(copyExprs, ", "), q.table(def.name)))
	}
	stmts = append(stmts,
		sqlStatement("DROP TABLE %s", q.table(def.name)),
		renameSQL(tmp, def.name))
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		stmts = append(stmts, statement{sql: createIndexSQL(q, def.name, idx, schemaOnIndex)})
	}
	return stmts, nil
}
