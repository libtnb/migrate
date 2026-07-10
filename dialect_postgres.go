package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Postgres is the PostgreSQL dialect.
var Postgres Dialect = postgresDialect{}

type postgresDialect struct{}

const pgQ = quoter('"')

func (postgresDialect) name() string           { return "postgres" }
func (postgresDialect) transactionalDDL() bool { return true }

func (postgresDialect) placeholder(n int) string { return fmt.Sprintf("$%d", n) }

func (postgresDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version VARCHAR(191) PRIMARY KEY,
	batch INTEGER NOT NULL,
	checksum CHAR(64) NOT NULL,
	applied_at VARCHAR(32) NOT NULL
)`, pgQ.table(table))
}

func (d postgresDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(pgQ, o)}, nil
	case *renameTable:
		// RENAME TO takes a bare name: renames stay within the schema.
		if schemaPrefix(o.from) != schemaPrefix(o.to) {
			return nil, fmt.Errorf("migrate: postgres cannot move table %q to %q with Rename; use Exec with ALTER TABLE ... SET SCHEMA", o.from, o.to)
		}
		return []statement{sqlStatement("ALTER TABLE %s RENAME TO %s", pgQ.table(o.from), pgQ.ident(baseName(o.to)))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		stmts, err := compileRecreate(d, pgQ, false, func(from, to string) statement {
			return sqlStatement("ALTER TABLE %s RENAME TO %s", pgQ.table(from), pgQ.ident(baseName(to)))
		}, d.listTriggers, o.def)
		if err != nil {
			return nil, err
		}
		return append(stmts, d.recreateEpilogue(o.def)...), nil
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: postgres: unsupported operation %T", op)
	}
}

func (d postgresDialect) compileCreate(def *tableDef) ([]statement, error) {
	pk, err := primaryColumns(def)
	if err != nil {
		return nil, err
	}

	clauses := make([]string, 0, len(def.columns)+len(def.fks)+1)
	var comments []statement
	for _, c := range def.columns {
		clause, err := d.columnSQL(def.constraintTable(), c)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
		if c.comment != "" {
			comments = append(comments, d.commentSQL(def.name, c))
		}
	}
	if len(pk) > 0 {
		if def.constraintBase != "" {
			// Recreate's temporary table: a named PK would create a backing
			// index colliding with the live table's (index names share the
			// schema namespace). Leave it unnamed — Postgres names it after
			// the temporary table — and rename it after the table swap.
			clauses = append(clauses, fmt.Sprintf("PRIMARY KEY (%s)", pgQ.idents(pk)))
		} else {
			clauses = append(clauses, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY (%s)",
				pgQ.ident(primaryName(def.name)), pgQ.idents(pk)))
		}
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(pgQ, chk))
	}
	for _, fk := range def.fks {
		clauses = append(clauses, foreignClause(pgQ, def.constraintTable(), fk))
	}

	stmts := []statement{sqlStatement("CREATE TABLE %s (\n\t%s\n)",
		pgQ.table(def.name), strings.Join(clauses, ",\n\t"))}
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		stmts = append(stmts, statement{sql: createIndexSQL(pgQ, def.name, idx, false)})
	}
	if def.comment != "" {
		stmts = append(stmts, d.tableCommentSQL(def.name, def.comment))
	}
	return append(stmts, comments...), nil
}

func (d postgresDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := pgQ.table(op.table)
	var stmts []statement
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			clause, err := d.columnSQL(op.table, c.col)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD COLUMN %s", table, clause))
			for _, idx := range inlineIndexes([]*columnDef{c.col}) {
				stmts = append(stmts, statement{sql: createIndexSQL(pgQ, op.table, idx, false)})
			}
			if c.col.comment != "" {
				stmts = append(stmts, d.commentSQL(op.table, c.col))
			}
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, pgQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s", table, pgQ.ident(c.from), pgQ.ident(c.to)))
		case *addIndex:
			stmts = append(stmts, statement{sql: createIndexSQL(pgQ, op.table, c.idx, false)})
		case *dropIndex:
			// Indexes live in the table's schema; dropping needs the same
			// qualification the table carries.
			stmts = append(stmts, sqlStatement("DROP INDEX %s", pgQ.table(schemaPrefix(op.table)+c.name)))
		case *addForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, foreignClause(pgQ, op.table, c.fk)))
		case *dropForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(c.name)))
		case *addPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY (%s)",
				table, pgQ.ident(primaryName(op.table)), pgQ.idents(c.columns)))
		case *dropPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(primaryName(op.table))))
		case *renameIndex:
			// The index lives in the table's schema; the new name stays bare.
			stmts = append(stmts, sqlStatement("ALTER INDEX %s RENAME TO %s",
				pgQ.table(schemaPrefix(op.table)+c.from), pgQ.ident(c.to)))
		case *setTableComment:
			stmts = append(stmts, d.tableCommentSQL(op.table, c.comment))
		case *addCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, checkClause(pgQ, c.chk)))
		case *dropCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CONSTRAINT %s", table, pgQ.ident(c.name)))
		default:
			return nil, fmt.Errorf("migrate: postgres: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

func (d postgresDialect) columnSQL(table string, c *columnDef) (string, error) {
	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(pgQ.ident(c.name) + " " + typ)
	if c.autoIncr {
		// The SQL-standard generated column, preferred over legacy serial.
		b.WriteString(" GENERATED BY DEFAULT AS IDENTITY")
	}
	if c.inlinePrimary() {
		// The inline PRIMARY KEY names its constraint {table}_pkey, matching
		// the conventional name used everywhere else.
		b.WriteString(" PRIMARY KEY")
		return b.String(), nil
	}
	b.WriteString(generatedClause(c))
	if !c.nullable {
		b.WriteString(" NOT NULL")
	}
	def, err := defaultClause(c, false, "CURRENT_TIMESTAMP")
	if err != nil {
		return "", err
	}
	b.WriteString(def)
	if c.kind == kindEnum {
		b.WriteString(enumCheckSQL(pgQ, table, c))
	}
	return b.String(), nil
}

func (postgresDialect) typeSQL(c *columnDef) (string, error) {
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString, kindEnum:
		return fmt.Sprintf("VARCHAR(%d)", charLength(c.length)), nil
	case kindChar:
		return fmt.Sprintf("CHAR(%d)", charLength(c.length)), nil
	case kindText:
		return "TEXT", nil
	case kindTinyInt, kindSmallInt:
		return "SMALLINT", nil
	case kindInt:
		return "INTEGER", nil
	case kindBigInt:
		return "BIGINT", nil
	case kindBool:
		return "BOOLEAN", nil
	case kindDecimal:
		return fmt.Sprintf("NUMERIC(%d, %d)", c.precision, c.scale), nil
	case kindFloat:
		return "REAL", nil
	case kindDouble:
		return "DOUBLE PRECISION", nil
	case kindDate:
		return "DATE", nil
	case kindTime:
		return "TIME", nil
	case kindDateTime, kindTimestamp:
		return "TIMESTAMP", nil
	case kindTimestampTz:
		return "TIMESTAMPTZ", nil
	case kindJSON:
		return "JSONB", nil
	case kindUUID:
		return "UUID", nil
	case kindBinary:
		return "BYTEA", nil
	default:
		return "", fmt.Errorf("migrate: postgres: unsupported column kind for %q", c.name)
	}
}

func (postgresDialect) commentSQL(table string, c *columnDef) statement {
	comment := strings.ReplaceAll(c.comment, "'", "''")
	return sqlStatement("COMMENT ON COLUMN %s.%s IS '%s'", pgQ.table(table), pgQ.ident(c.name), comment)
}

// listTriggers returns the user triggers attached to the table as complete
// CREATE TRIGGER statements. Internal triggers (foreign key enforcement)
// belong to their constraints and rebuild with them.
func (postgresDialect) listTriggers(ctx context.Context, db DB, table string) ([]string, error) {
	return queryStrings(ctx, db,
		"SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal ORDER BY tgname",
		pgQ.table(table))
}

// recreateEpilogue finishes a table swap: the rebuilt primary key kept the
// temporary table's auto-generated constraint name, and a copied identity
// column still has its sequence at the start.
func (d postgresDialect) recreateEpilogue(def *tableDef) []statement {
	var stmts []statement
	pk, err := primaryColumns(def)
	hasInline := false
	for _, c := range def.columns {
		if c.inlinePrimary() {
			hasInline = true
		}
	}
	if err == nil && (len(pk) > 0 || hasInline) {
		tmpPkey := baseName(def.name) + "__migrate_new_pkey"
		stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME CONSTRAINT %s TO %s",
			pgQ.table(def.name), pgQ.ident(tmpPkey), pgQ.ident(primaryName(def.name))))
	}
	for _, c := range def.columns {
		if !c.inlinePrimary() || c.skipCopy {
			continue
		}
		// Explicit-id inserts do not advance an identity sequence; without
		// this the next application insert would collide with copied rows.
		table := strings.ReplaceAll(pgQ.table(def.name), "'", "''")
		column := strings.ReplaceAll(c.name, "'", "''")
		stmts = append(stmts, sqlStatement(
			"SELECT setval(pg_get_serial_sequence('%s', '%s'), COALESCE((SELECT MAX(%s) FROM %s), 0) + 1, false)",
			table, column, pgQ.ident(c.name), pgQ.table(def.name)))
	}
	return stmts
}

// Advisory locking: the lock key is derived inside Postgres itself from the
// current database and the records table, so concurrent migrators of the same
// database exclude each other while other databases on the cluster are
// unaffected. Session-level locks release automatically when the connection
// drops, and unlike transaction-level ones they do not hold a snapshot that
// would block CREATE INDEX CONCURRENTLY elsewhere.
const pgLockKey = "hashtextextended('go-rio/migrate:' || current_database() || ':' || $1, 0)"

func (postgresDialect) lock(ctx context.Context, conn *sql.Conn, table string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock("+pgLockKey+")", table).Scan(&acquired); err != nil {
			return fmt.Errorf("migrate: acquire advisory lock: %w", err)
		}
		if acquired {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: waited %v for the advisory lock", ErrLockTimeout, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (postgresDialect) unlock(ctx context.Context, conn *sql.Conn, table string) error {
	var released bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock("+pgLockKey+")", table).Scan(&released); err != nil {
		return fmt.Errorf("migrate: release advisory lock: %w", err)
	}
	if !released {
		return fmt.Errorf("migrate: advisory lock was not held at release time")
	}
	return nil
}

// dropTableSQL is shared by the dialects, which agree on DROP TABLE syntax.
func dropTableSQL(q quoter, o *dropTable) statement {
	ifExists := ""
	if o.ifExists {
		ifExists = "IF EXISTS "
	}
	return sqlStatement("DROP TABLE %s%s", ifExists, q.table(o.name))
}

func (postgresDialect) quoteIdent(name string) string { return pgQ.table(name) }

func (postgresDialect) tableCommentSQL(table, comment string) statement {
	return sqlStatement("COMMENT ON TABLE %s IS '%s'", pgQ.table(table), strings.ReplaceAll(comment, "'", "''"))
}

func (postgresDialect) listTablesSQL() string {
	return "SELECT tablename FROM pg_tables WHERE schemaname = current_schema()"
}

// CASCADE also removes dependent objects such as views, which plain table
// drops would trip over.
func (postgresDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", pgQ.table(table))
}
