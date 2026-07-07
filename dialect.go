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
		q.ident(table+"_"+c.name+"_check"), q.ident(c.name), strings.Join(vals, ", "))
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
		q.ident(fk.resolvedName(table)), q.idents(fk.columns), q.ident(fk.refTable), q.idents(refCols))
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
func createIndexSQL(q quoter, table string, idx *indexDef) string {
	unique := ""
	if idx.unique {
		unique = "UNIQUE "
	}
	return fmt.Sprintf("CREATE %sINDEX %s ON %s (%s)",
		unique, q.ident(idx.resolvedName(table)), q.ident(table), q.idents(idx.columns))
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
