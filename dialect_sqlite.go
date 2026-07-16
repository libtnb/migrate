package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// SQLite is the SQLite dialect. It expects SQLite 3.35 or newer for
// ALTER TABLE DROP COLUMN.
//
// SQLite cannot alter constraints after a table exists: adding or dropping
// foreign keys and primary keys compiles to a clear error rather than
// silently skipping the change — declare them when creating the table, or
// change them with Schema.Recreate, which rebuilds the table while keeping
// its rows. Advisory locking is a no-op: the single-writer database file
// serializes the migration transactions themselves, and each migration
// records itself as its transaction's first write, so a racing migrator
// loses on the records table — cleanly, before touching the schema — rather
// than halfway through with "already exists".
var SQLite Dialect = sqliteDialect{}

type sqliteDialect struct{}

const liteQ = quoter('"')

func (sqliteDialect) name() string           { return "sqlite" }
func (sqliteDialect) transactionalDDL() bool { return true }

func (sqliteDialect) placeholder(int) string { return "?" }

func (sqliteDialect) ensureTableSQL(table string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
	version TEXT PRIMARY KEY,
	batch INTEGER NOT NULL,
	checksum TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`, liteQ.table(table))
}

func (d sqliteDialect) compile(op operation) ([]statement, error) {
	switch o := op.(type) {
	case *createTable:
		return d.compileCreate(o.def)
	case *dropTable:
		return []statement{dropTableSQL(liteQ, o)}, nil
	case *renameTable:
		// RENAME TO takes a bare name: renames stay within the schema.
		if schemaPrefix(o.from) != schemaPrefix(o.to) {
			return nil, fmt.Errorf("migrate: sqlite cannot move table %q to %q with Rename; tables stay in their attached database", o.from, o.to)
		}
		return []statement{sqlStatement("ALTER TABLE %s RENAME TO %s", liteQ.table(o.from), liteQ.ident(baseName(o.to)))}, nil
	case *alterTable:
		return d.compileAlter(o)
	case *recreateTable:
		return compileRecreate(d, liteQ, true, func(from, to string) statement {
			return sqlStatement("ALTER TABLE %s RENAME TO %s", liteQ.table(from), liteQ.ident(baseName(to)))
		}, d.listTriggers, o.def)
	case *rawSQL:
		return []statement{{sql: o.sql, args: o.args}}, nil
	case *goFunc:
		return []statement{{fn: o.fn}}, nil
	default:
		return nil, fmt.Errorf("migrate: sqlite: unsupported operation %T", op)
	}
}

func (d sqliteDialect) compileCreate(def *tableDef) ([]statement, error) {
	pk, err := primaryColumns(def)
	if err != nil {
		return nil, err
	}

	clauses := make([]string, 0, len(def.columns)+len(def.fks)+1)
	for _, c := range def.columns {
		clause, err := d.columnSQL(def.constraintTable(), c)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	if len(pk) > 0 {
		clauses = append(clauses, fmt.Sprintf("CONSTRAINT %s PRIMARY KEY (%s)",
			liteQ.ident(primaryName(def.constraintTable())), liteQ.idents(pk)))
	}
	for _, chk := range def.checks {
		clauses = append(clauses, checkClause(liteQ, chk))
	}
	for _, fk := range def.fks {
		clauses = append(clauses, foreignClause(liteQ, def.constraintTable(), fk))
	}

	stmts := []statement{sqlStatement("CREATE TABLE %s (\n\t%s\n)",
		liteQ.table(def.name), strings.Join(clauses, ",\n\t"))}
	for _, idx := range append(inlineIndexes(def.columns), def.indexes...) {
		sql, err := createIndexSQL("sqlite", liteQ, def.name, idx, true)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, statement{sql: sql})
	}
	return stmts, nil
}

func (d sqliteDialect) compileAlter(op *alterTable) ([]statement, error) {
	table := liteQ.table(op.table)
	var stmts []statement
	for _, ch := range op.changes {
		switch c := ch.(type) {
		case *addColumn:
			if c.col.change {
				return nil, fmt.Errorf("migrate: sqlite cannot change column %q of table %q; use Schema.Recreate", c.col.name, op.table)
			}
			if c.col.autoIncr {
				return nil, fmt.Errorf("migrate: sqlite cannot add auto-increment column %q to existing table %q; declare it in Create, or use Schema.Recreate", c.col.name, op.table)
			}
			if c.col.generatedExpr != "" && !c.col.generatedVirtual {
				// ALTER TABLE ADD COLUMN ... STORED fails on any populated table.
				return nil, fmt.Errorf("migrate: sqlite cannot add STORED generated column %q to existing table %q; use VirtualAs, or Schema.Recreate", c.col.name, op.table)
			}
			if c.col.useCurrent || c.col.defaultExpr != "" {
				// Non-constant defaults are rejected by ADD COLUMN on any
				// populated table.
				return nil, fmt.Errorf("migrate: sqlite cannot add column %q with a non-constant default to existing table %q; use a literal Default, or Schema.Recreate", c.col.name, op.table)
			}
			clause, err := d.columnSQL(op.table, c.col)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, sqlStatement("ALTER TABLE %s ADD COLUMN %s", table, clause))
			for _, idx := range inlineIndexes([]*columnDef{c.col}) {
				sql, err := createIndexSQL("sqlite", liteQ, op.table, idx, true)
				if err != nil {
					return nil, err
				}
				stmts = append(stmts, statement{sql: sql})
			}
		case *dropColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s DROP COLUMN %s", table, liteQ.ident(c.name)))
		case *renameColumn:
			stmts = append(stmts, sqlStatement("ALTER TABLE %s RENAME COLUMN %s TO %s", table, liteQ.ident(c.from), liteQ.ident(c.to)))
		case *addIndex:
			sql, err := createIndexSQL("sqlite", liteQ, op.table, c.idx, true)
			if err != nil {
				return nil, err
			}
			stmts = append(stmts, statement{sql: sql})
		case *dropIndex:
			stmts = append(stmts, sqlStatement("DROP INDEX %s", liteQ.table(schemaPrefix(op.table)+c.name)))
		case *renameIndex:
			return nil, fmt.Errorf("migrate: sqlite cannot rename index %q; drop it and declare a new one", c.from)
		case *setTableComment:
			// SQLite has no table comments; the declaration is documentation.
		case *addCheck:
			return nil, fmt.Errorf("migrate: sqlite cannot add a check constraint to existing table %q; declare it in Create, or use Schema.Recreate", op.table)
		case *dropCheck:
			return nil, fmt.Errorf("migrate: sqlite cannot drop a check constraint from table %q; use Schema.Recreate", op.table)
		case *addForeign:
			return nil, fmt.Errorf("migrate: sqlite cannot add a foreign key to existing table %q; declare it in Create, or use Schema.Recreate", op.table)
		case *dropForeign:
			return nil, fmt.Errorf("migrate: sqlite cannot drop a foreign key from table %q; use Schema.Recreate", op.table)
		case *addPrimary:
			return nil, fmt.Errorf("migrate: sqlite cannot add a primary key to existing table %q; declare it in Create, or use Schema.Recreate", op.table)
		case *dropPrimary:
			return nil, fmt.Errorf("migrate: sqlite cannot drop the primary key of table %q; use Schema.Recreate", op.table)
		default:
			return nil, fmt.Errorf("migrate: sqlite: unsupported change %T", ch)
		}
	}
	return stmts, nil
}

func (d sqliteDialect) columnSQL(table string, c *columnDef) (string, error) {
	typ, err := d.typeSQL(c)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(liteQ.ident(c.name) + " " + typ)
	if c.inlinePrimary() {
		if c.kind == kindRaw && !strings.EqualFold(strings.TrimSpace(c.rawType), "INTEGER") {
			return "", fmt.Errorf("migrate: sqlite allows AUTOINCREMENT only on INTEGER columns; column %q declares %q", c.name, c.rawType)
		}
		// INTEGER PRIMARY KEY aliases the rowid; AUTOINCREMENT prevents ids
		// of deleted rows from being reused.
		b.WriteString(" PRIMARY KEY AUTOINCREMENT")
		return b.String(), nil
	}
	if !c.nullable {
		b.WriteString(" NOT NULL")
	}
	b.WriteString(generatedClause(c))
	def, err := defaultClause(c, false, "CURRENT_TIMESTAMP")
	if err != nil {
		return "", err
	}
	b.WriteString(def)
	if c.kind == kindEnum {
		b.WriteString(enumCheckSQL(liteQ, table, c))
	}
	return b.String(), nil
}

// typeSQL names types for their affinity. SQLite does not enforce lengths or
// precision, but keeping them in the declaration documents intent and guides
// tools that read the schema.
func (sqliteDialect) typeSQL(c *columnDef) (string, error) {
	switch c.kind {
	case kindRaw:
		return c.rawType, nil
	case kindString:
		return fmt.Sprintf("VARCHAR(%d)", charLength(c.length)), nil
	case kindChar:
		return fmt.Sprintf("CHAR(%d)", charLength(c.length)), nil
	case kindText, kindEnum:
		return "TEXT", nil
	case kindTinyInt, kindSmallInt, kindInt, kindBigInt:
		return "INTEGER", nil
	case kindBool:
		return "BOOLEAN", nil
	case kindDecimal:
		return fmt.Sprintf("NUMERIC(%d, %d)", c.precision, c.scale), nil
	case kindFloat, kindDouble:
		return "REAL", nil
	case kindDate:
		return "DATE", nil
	case kindTime:
		return "TIME", nil
	case kindDateTime, kindTimestamp, kindTimestampTz:
		return "DATETIME", nil
	case kindJSON:
		return "TEXT", nil
	case kindUUID:
		return "CHAR(36)", nil
	case kindBinary:
		return "BLOB", nil
	default:
		return "", fmt.Errorf("migrate: sqlite: unsupported column kind for %q", c.name)
	}
}

// listTriggers returns the CREATE TRIGGER statements attached to the table,
// read from the sqlite_master of the table's schema. The stored SQL is the
// original DDL, replayable verbatim as long as the table keeps its name.
func (sqliteDialect) listTriggers(ctx context.Context, db DB, table string) ([]string, error) {
	master := "sqlite_master"
	if p := schemaPrefix(table); p != "" {
		master = liteQ.ident(strings.TrimSuffix(p, ".")) + "." + master
	}
	return queryStrings(ctx, db,
		"SELECT sql FROM "+master+" WHERE type = 'trigger' AND tbl_name = ? ORDER BY name", baseName(table))
}

func (sqliteDialect) lock(context.Context, *sql.Conn, string, time.Duration) error { return nil }
func (sqliteDialect) unlock(context.Context, *sql.Conn, string) error              { return nil }

func (sqliteDialect) quoteIdent(name string) string { return liteQ.table(name) }

func (sqliteDialect) listTablesSQL() string {
	return "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'"
}

func (sqliteDialect) freshDropSQL(table string) string {
	return fmt.Sprintf("DROP TABLE IF EXISTS %s", liteQ.table(table))
}
