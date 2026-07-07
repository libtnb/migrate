package migrate

import (
	"context"
	"fmt"
	"strings"
)

// operation is a single step recorded by a migration function. Operations are
// plain values: the same declaration compiles to dialect SQL, reverses itself
// for rollbacks, and hashes into the migration checksum.
type operation interface {
	// inverse returns the operation that undoes this one, or an error
	// wrapping ErrIrreversible when the information needed to reverse it
	// has been discarded (e.g. dropping a table loses its definition).
	inverse() (operation, error)
}

func irreversible(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrIrreversible, fmt.Sprintf(format, a...))
}

// colKind enumerates the portable column types. Dialects map each kind to
// their native type; kindRaw passes a user-supplied type through verbatim.
type colKind int

const (
	kindRaw colKind = iota
	kindString
	kindChar
	kindText
	kindTinyInt
	kindSmallInt
	kindInt
	kindBigInt
	kindBool
	kindDecimal
	kindFloat
	kindDouble
	kindDate
	kindTime
	kindDateTime
	kindTimestamp
	kindTimestampTz
	kindJSON
	kindUUID
	kindBinary
	kindEnum
)

type columnDef struct {
	name      string
	kind      colKind
	rawType   string // kindRaw only
	length    int    // string/char/binary
	precision int    // decimal/float/time precision
	scale     int    // decimal
	enumVals  []string

	unsigned bool
	autoIncr bool
	primary  bool

	nullable           bool
	hasDefault         bool
	defaultVal         any
	defaultExpr        string
	useCurrent         bool
	useCurrentOnUpdate bool

	unique  bool
	indexed bool
	comment string
	after   string
	first   bool
}

// integerKind reports whether the column type can auto-increment.
func (c *columnDef) integerKind() bool {
	switch c.kind {
	case kindTinyInt, kindSmallInt, kindInt, kindBigInt:
		return true
	default:
		return false
	}
}

// inlinePrimary marks the auto-incrementing primary key, which every dialect
// renders inline with the column (SQLite requires it, the others read best
// that way) instead of as a table-level constraint.
func (c *columnDef) inlinePrimary() bool {
	return c.autoIncr && c.primary
}

type indexDef struct {
	name    string // empty means the conventional name
	columns []string
	unique  bool
}

type foreignDef struct {
	name       string // empty means the conventional name
	columns    []string
	refTable   string
	refColumns []string
	onDelete   string
	onUpdate   string
}

// tableDef is the full declaration collected by a Create call.
type tableDef struct {
	name    string
	columns []*columnDef
	indexes []*indexDef
	fks     []*foreignDef
	primary []string // composite primary key columns
	comment string
	errs    []error // declaration mistakes, surfaced at compile time
}

// Conventional constraint names, shared by every dialect so that dropping by
// columns can reconstruct the name that adding by columns produced.

func indexName(table string, columns []string, unique bool) string {
	suffix := "index"
	if unique {
		suffix = "unique"
	}
	return table + "_" + strings.Join(columns, "_") + "_" + suffix
}

func foreignName(table string, columns []string) string {
	return table + "_" + strings.Join(columns, "_") + "_foreign"
}

func primaryName(table string) string {
	return table + "_pkey"
}

func (i *indexDef) resolvedName(table string) string {
	if i.name != "" {
		return i.name
	}
	return indexName(table, i.columns, i.unique)
}

func (f *foreignDef) resolvedName(table string) string {
	if f.name != "" {
		return f.name
	}
	return foreignName(table, f.columns)
}

// --- top-level operations ---

type createTable struct {
	def *tableDef
}

func (o *createTable) inverse() (operation, error) {
	return &dropTable{name: o.def.name}, nil
}

type dropTable struct {
	name     string
	ifExists bool
}

func (o *dropTable) inverse() (operation, error) {
	return nil, irreversible("dropping table %q discards its definition", o.name)
}

type renameTable struct {
	from, to string
}

func (o *renameTable) inverse() (operation, error) {
	return &renameTable{from: o.to, to: o.from}, nil
}

type alterTable struct {
	table   string
	changes []change
	errs    []error
}

func (o *alterTable) inverse() (operation, error) {
	inv := &alterTable{table: o.table, changes: make([]change, 0, len(o.changes))}
	for i := len(o.changes) - 1; i >= 0; i-- {
		cs, err := o.changes[i].inverseChange(o.table)
		if err != nil {
			return nil, err
		}
		inv.changes = append(inv.changes, cs...)
	}
	return inv, nil
}

type rawSQL struct {
	sql  string
	args []any
}

func (o *rawSQL) inverse() (operation, error) {
	return nil, irreversible("raw SQL cannot be reversed; declare an explicit down with WithDown")
}

type goFunc struct {
	fn func(context.Context, DB) error
}

func (o *goFunc) inverse() (operation, error) {
	return nil, irreversible("a Go function cannot be reversed; declare an explicit down with WithDown")
}

// --- alter-table changes ---

// change is a single mutation inside an alterTable operation. Changes reverse
// individually — possibly into several changes — and alterTable reverses them
// in reverse order.
type change interface {
	inverseChange(table string) ([]change, error)
}

type addColumn struct {
	col *columnDef
}

func (c *addColumn) inverseChange(table string) ([]change, error) {
	// Indexes implied by Unique/Index modifiers drop before the column does:
	// Postgres and MySQL would cascade them away, but SQLite refuses to drop
	// a column that an index still references.
	var out []change
	for _, idx := range inlineIndexes([]*columnDef{c.col}) {
		out = append(out, &dropIndex{name: idx.resolvedName(table)})
	}
	return append(out, &dropColumn{name: c.col.name}), nil
}

type dropColumn struct {
	name string
}

func (c *dropColumn) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping column %q of table %q discards its definition", c.name, table)
}

type renameColumn struct {
	from, to string
}

func (c *renameColumn) inverseChange(string) ([]change, error) {
	return []change{&renameColumn{from: c.to, to: c.from}}, nil
}

type addIndex struct {
	idx *indexDef
}

func (c *addIndex) inverseChange(table string) ([]change, error) {
	return []change{&dropIndex{name: c.idx.resolvedName(table)}}, nil
}

type dropIndex struct {
	name string
}

func (c *dropIndex) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping index %q of table %q discards its definition", c.name, table)
}

type addForeign struct {
	fk *foreignDef
}

func (c *addForeign) inverseChange(table string) ([]change, error) {
	return []change{&dropForeign{name: c.fk.resolvedName(table)}}, nil
}

type dropForeign struct {
	name string
}

func (c *dropForeign) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping foreign key %q of table %q discards its definition", c.name, table)
}

type addPrimary struct {
	columns []string
}

func (c *addPrimary) inverseChange(string) ([]change, error) {
	return []change{&dropPrimary{}}, nil
}

type dropPrimary struct{}

func (c *dropPrimary) inverseChange(table string) ([]change, error) {
	return nil, irreversible("dropping the primary key of table %q discards its definition", table)
}

type renameIndex struct {
	from, to string
}

func (c *renameIndex) inverseChange(string) ([]change, error) {
	return []change{&renameIndex{from: c.to, to: c.from}}, nil
}

type setTableComment struct {
	comment string
}

func (c *setTableComment) inverseChange(table string) ([]change, error) {
	return nil, irreversible("changing the comment of table %q discards the previous comment", table)
}
