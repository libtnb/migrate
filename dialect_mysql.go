package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MySQL is the MySQL dialect. It expects MySQL 8.0 or newer (or an equivalent
// MariaDB release) for RENAME COLUMN and expression defaults.
//
// MySQL commits implicitly around every DDL statement, so unlike Postgres and
// SQLite a failed migration cannot roll back the DDL it already executed; the
// error identifies the failing statement so the database can be reconciled.
var MySQL Dialect = mysqlDialect{}

type mysqlDialect struct{}

const myQ = quoter('`')

func (mysqlDialect) name() string           { return "mysql" }
func (mysqlDialect) transactionalDDL() bool { return false }

func (mysqlDialect) placeholder(int) string { return "?" }

func (mysqlDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (\n"+
		"\tversion VARCHAR(191) PRIMARY KEY,\n"+
		"\tbatch INTEGER NOT NULL,\n"+
		"\tchecksum CHAR(64) NOT NULL,\n"+
		"\tapplied_at VARCHAR(32) NOT NULL\n"+
		")", myQ.table(table))
}

func (d mysqlDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(myQ, o)}, nil
	case *renameTable:
		return []statement{sqlStatement("RENAME TABLE %s TO %s", myQ.table(o.from), myQ.table(o.to))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		// MySQL commits every DDL statement implicitly, so the copy-drop-
		// rename sequence cannot be made atomic: a crash between the DROP and
		// the RENAME leaves the live table gone. An atomic two-way RENAME
		// swap does not help either — child foreign keys follow the renamed
		// table to its backup name. MySQL's native ALTER TABLE covers every
		// Recreate use case (MODIFY COLUMN, constraint changes), so refusing
		// is strictly safer than a destructive window.
		return nil, fmt.Errorf("migrate: mysql cannot rebuild table %q atomically (DDL commits implicitly); use Schema.Table with native ALTER operations, or Exec", o.def.name)
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: mysql: unsupported operation %T", op)
	}
}

func (d mysqlDialect) compileCreate(def *tableDef) ([]statement, error) {
	pk, err := primaryColumns(def)
	if err != nil {
		return nil, err
	}

	clauses := make([]string, 0, len(def.columns)+len(def.fks)+1)
	for _, c := range def.columns {
		clause, err := d.columnSQL(c, false)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	if len(pk) > 0 {
		// MySQL names every primary key PRIMARY; a constraint name would be
		// accepted and discarded.
		clauses = append(clauses, fmt.Sprintf("PRIMARY KEY (%s)", myQ.idents(pk)))
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(myQ, chk))
	}
	for _, fk := range def.fks {
		clauses = append(clauses, foreignClause(myQ, def.constraintTable(), fk))
	}

	suffix := ""
	if def.comment != "" {
		suffix = " COMMENT = '" + mysqlEscape(def.comment) + "'"
	}
	stmts := []statement{sqlStatement("CREATE TABLE %s (\n\t%s\n)%s",
		myQ.table(def.name), strings.Join(clauses, ",\n\t"), suffix)}
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		stmts = append(stmts, statement{sql: createIndexSQL(myQ, def.name, idx, false)})
	}
	return stmts, nil
}

func (d mysqlDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := myQ.table(op.table)
	var stmts []statement
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			clause, err := d.columnSQL(c.col, true)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD COLUMN %s", table, clause))
			for _, idx := range inlineIndexes([]*columnDef{c.col}) {
				stmts = append(stmts, statement{sql: createIndexSQL(myQ, op.table, idx, false)})
			}
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, myQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s", table, myQ.ident(c.from), myQ.ident(c.to)))
		case *addIndex:
			stmts = append(stmts, statement{sql: createIndexSQL(myQ, op.table, c.idx, false)})
		case *dropIndex:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP INDEX %s", table, myQ.ident(c.name)))
		case *addForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, foreignClause(myQ, op.table, c.fk)))
		case *dropForeign:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP FOREIGN KEY %s", table, myQ.ident(c.name)))
		case *addPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD PRIMARY KEY (%s)", table, myQ.idents(c.columns)))
		case *dropPrimary:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP PRIMARY KEY", table))
		case *renameIndex:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME INDEX %s TO %s", table, myQ.ident(c.from), myQ.ident(c.to)))
		case *setTableComment:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s COMMENT = '%s'", table, mysqlEscape(c.comment)))
		case *addCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD %s", table, checkClause(myQ, c.chk)))
		case *dropCheck:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP CHECK %s", table, myQ.ident(c.name)))
		default:
			return nil, fmt.Errorf("migrate: mysql: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

// columnSQL renders a column definition. Position modifiers (After, First)
// only apply when altering; CREATE TABLE already places columns in
// declaration order.
func (d mysqlDialect) columnSQL(c *columnDef, altering bool) (string, error) {
	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(myQ.ident(c.name) + " " + typ)
	b.WriteString(generatedClause(c))
	if !c.nullable {
		b.WriteString(" NOT NULL")
	}
	def, err := defaultClause(c, true, "CURRENT_TIMESTAMP(6)")
	if err != nil {
		return "", err
	}
	b.WriteString(def)
	if c.useCurrentOnUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP(6)")
	}
	if c.autoIncr {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.inlinePrimary() {
		b.WriteString(" PRIMARY KEY")
	}
	if c.comment != "" {
		b.WriteString(" COMMENT '" + mysqlEscape(c.comment) + "'")
	}
	if altering {
		switch {
		case c.first:
			b.WriteString(" FIRST")
		case c.after != "":
			b.WriteString(" AFTER " + myQ.ident(c.after))
		}
	}
	return b.String(), nil
}

func (mysqlDialect) typeSQL(c *columnDef) (string, error) {
	unsigned := func(t string) string {
		if c.unsigned {
			return t + " UNSIGNED"
		}
		return t
	}
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString:
		return fmt.Sprintf("VARCHAR(%d)", charLength(c.length)), nil
	case kindChar:
		return fmt.Sprintf("CHAR(%d)", charLength(c.length)), nil
	case kindText:
		// MySQL's TEXT caps at 64 KB; LONGTEXT matches the unbounded text
		// type of the other dialects.
		return "LONGTEXT", nil
	case kindTinyInt:
		return unsigned("TINYINT"), nil
	case kindSmallInt:
		return unsigned("SMALLINT"), nil
	case kindInt:
		return unsigned("INT"), nil
	case kindBigInt:
		return unsigned("BIGINT"), nil
	case kindBool:
		return "TINYINT(1)", nil
	case kindDecimal:
		return fmt.Sprintf("DECIMAL(%d, %d)", c.precision, c.scale), nil
	case kindFloat:
		return "FLOAT", nil
	case kindDouble:
		return "DOUBLE", nil
	case kindDate:
		return "DATE", nil
	case kindTime:
		return "TIME", nil
	case kindDateTime, kindTimestamp, kindTimestampTz:
		// DATETIME avoids TIMESTAMP's 2038 horizon and implicit-default
		// magic; microsecond precision matches the other dialects.
		return "DATETIME(6)", nil
	case kindJSON:
		return "JSON", nil
	case kindUUID:
		return "CHAR(36)", nil
	case kindBinary:
		return "BLOB", nil
	case kindEnum:
		vals := make([]string, len(c.enumVals))
		for i, v := range c.enumVals {
			vals[i] = "'" + mysqlEscape(v) + "'"
		}
		return "ENUM(" + strings.Join(vals, ", ") + ")", nil
	default:
		return "", fmt.Errorf("migrate: mysql: unsupported column kind for %q", c.name)
	}
}

// Advisory locking via GET_LOCK. The lock name mixes in the current schema so
// migrators of different databases on one server do not exclude each other;
// hashing keeps it under GET_LOCK's 64-character limit. Session locks release
// automatically when the connection drops.
const myLockName = "CONCAT('go-rio.migrate.', MD5(CONCAT(IFNULL(DATABASE(), ''), ':', ?)))"

func (mysqlDialect) lock(ctx context.Context, conn *sql.Conn, table string, timeout time.Duration) error {
	// GET_LOCK counts whole seconds; round up so a sub-second timeout still
	// waits instead of degrading to a single non-blocking attempt.
	seconds := int64((timeout + time.Second - 1) / time.Second)
	var acquired sql.NullInt64
	err := conn.QueryRowContext(ctx, "SELECT GET_LOCK("+myLockName+", ?)", table, seconds).Scan(&acquired)
	if err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return fmt.Errorf("%w: waited %v for the advisory lock", ErrLockTimeout, timeout)
	}
	return nil
}

func (mysqlDialect) unlock(ctx context.Context, conn *sql.Conn, table string) error {
	var released sql.NullInt64
	err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK("+myLockName+")", table).Scan(&released)
	if err != nil {
		return fmt.Errorf("migrate: release advisory lock: %w", err)
	}
	if !released.Valid || released.Int64 != 1 {
		return fmt.Errorf("migrate: advisory lock was not held at release time")
	}
	return nil
}

func (mysqlDialect) quoteIdent(name string) string { return myQ.table(name) }

// mysqlEscape escapes a string for a single-quoted MySQL literal, where
// backslash is an escape character unless NO_BACKSLASH_ESCAPES is set.
func mysqlEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", "''")
}

func (mysqlDialect) listTablesSQL() string {
	return "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'"
}

func (mysqlDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s", myQ.table(table))
}
