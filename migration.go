package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Sentinel errors. Failures wrap these, so errors.Is works across the
// additional context.
var (
	// ErrIrreversible marks a rollback of a migration that cannot be
	// reversed automatically and declares no explicit down.
	ErrIrreversible = errors.New("migrate: migration cannot be automatically reversed")
	// ErrLockTimeout marks a failure to acquire the advisory lock, meaning
	// another migrator held it for the whole wait.
	ErrLockTimeout = errors.New("migrate: timed out waiting for the migration lock")
	// ErrChecksumMismatch marks an applied migration whose declaration no
	// longer produces the SQL it was applied with.
	ErrChecksumMismatch = errors.New("migrate: checksum mismatch")
)

// Migration is a registered migration: a name that orders and identifies it,
// and a declaration function. Values are created by Add or AddRepeatable and
// immutable afterwards.
type Migration struct {
	name       string
	up         func(*Schema)
	down       func(*Schema) // nil means derive by reversing up
	useTx      bool
	repeatable bool
	assured    bool // reviewed: skip the safety analysis
}

// Name returns the migration's registered name.
func (m *Migration) Name() string { return m.name }

// MigrationOption configures a single migration at registration time.
type MigrationOption func(*Migration)

// WithDown declares an explicit down migration, needed when up records
// irreversible operations (Drop, DropColumn, Exec, Run) and the migration
// should still be able to roll back.
func WithDown(down func(*Schema)) MigrationOption {
	return func(m *Migration) { m.down = down }
}

// WithoutTransaction runs the migration outside a transaction, required for
// statements that refuse to run inside one, such as Postgres's
// CREATE INDEX CONCURRENTLY. Without a transaction a mid-migration failure
// leaves earlier statements applied — keep such migrations to a single
// statement where possible.
func WithoutTransaction() MigrationOption {
	return func(m *Migration) { m.useTx = false }
}

// upOps runs the declaration function and returns the recorded operations.
func (m *Migration) upOps() ([]operation, error) {
	s := &Schema{}
	m.up(s)
	return s.ops, validateSchema(m.name, s)
}

// downOps returns the operations that roll the migration back: the explicit
// down when declared, otherwise the up operations reversed in reverse order.
func (m *Migration) downOps() ([]operation, error) {
	if m.down != nil {
		s := &Schema{}
		m.down(s)
		return s.ops, validateSchema(m.name, s)
	}
	ups, err := m.upOps()
	if err != nil {
		return nil, err
	}
	downs := make([]operation, 0, len(ups))
	for i := len(ups) - 1; i >= 0; i-- {
		inv, err := ups[i].inverse()
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", m.name, err)
		}
		downs = append(downs, inv)
	}
	return downs, nil
}

// validateSchema surfaces declaration mistakes collected while recording.
func validateSchema(name string, s *Schema) error {
	errs := append([]error(nil), s.errs...)
	check := func(table string, cols []*columnDef) {
		for _, c := range cols {
			if c.autoIncr {
				switch {
				case !c.integerKind() && c.kind != kindRaw:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q must be an integer", c.name, table))
				case c.hasDefault:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q cannot have a default value", c.name, table))
				case c.nullable:
					errs = append(errs, fmt.Errorf("auto-increment column %q of table %q cannot be nullable", c.name, table))
				}
			}
			if c.generatedExpr != "" && (c.hasDefault || c.useCurrent || c.autoIncr) {
				errs = append(errs, fmt.Errorf("generated column %q of table %q cannot combine with defaults or auto-increment", c.name, table))
			}
			if c.copyFrom != "" && c.skipCopy {
				errs = append(errs, fmt.Errorf("column %q of table %q declares both CopyFrom and SkipCopy", c.name, table))
			}
			if c.primary && c.nullable {
				// SQLite would honour the contradiction and accept NULL keys.
				errs = append(errs, fmt.Errorf("primary key column %q of table %q cannot be nullable", c.name, table))
			}
		}
	}
	checkTablePK := func(def *tableDef) {
		nullable := make(map[string]bool, len(def.columns))
		for _, c := range def.columns {
			nullable[c.name] = c.nullable
		}
		for _, p := range def.primary {
			if nullable[p] {
				errs = append(errs, fmt.Errorf("primary key column %q of table %q cannot be nullable", p, def.name))
			}
		}
	}
	for _, op := range s.ops {
		switch o := op.(type) {
		case *createTable:
			errs = append(errs, o.def.errs...)
			check(o.def.name, o.def.columns)
			checkTablePK(o.def)
		case *recreateTable:
			errs = append(errs, o.def.errs...)
			check(o.def.name, o.def.columns)
			checkTablePK(o.def)
		case *alterTable:
			errs = append(errs, o.errs...)
			for _, ch := range o.changes {
				if add, ok := ch.(*addColumn); ok {
					check(o.table, []*columnDef{add.col})
				}
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("migration %q: %w", name, declarationErrors(errs))
}

// checksum fingerprints the migration as the SQL it compiles to under the
// given dialect. Statement text and raw arguments participate; the body of a
// Run function cannot, so edits to Go logic are not detectable.
func (m *Migration) checksum(d Dialect) (string, error) {
	stmts, err := m.compile(d, true)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, s := range stmts {
		if s.fn != nil {
			h.Write([]byte("<go>\x00"))
			continue
		}
		h.Write([]byte(s.sql))
		for _, a := range s.args {
			_, _ = fmt.Fprintf(h, "\x00%v", a)
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// compile renders the migration to statements for the dialect.
func (m *Migration) compile(d Dialect, up bool) ([]statement, error) {
	ops, err := m.upOps()
	if !up {
		ops, err = m.downOps()
	}
	if err != nil {
		return nil, err
	}
	if !m.useTx {
		for _, op := range ops {
			if _, ok := op.(*recreateTable); ok {
				// Without the transaction, a failure between the DROP and the
				// rename leaves the live table gone — the exact window the
				// MySQL gate exists for.
				return nil, fmt.Errorf("migration %q: Recreate requires the migration's transaction; keep WithoutTransaction statements in a separate migration", m.name)
			}
		}
	}
	var stmts []statement
	for _, op := range ops {
		s, err := d.compile(op)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", m.name, err)
		}
		stmts = append(stmts, s...)
	}
	return stmts, nil
}

// Collection is an ordered, named set of migrations. The package-level Add
// registers into a default collection, which suits the common one-app layout;
// tests and libraries embedding several migration sets can keep explicit
// collections instead.
type Collection struct {
	mu     sync.Mutex
	byName map[string]*Migration
}

// NewCollection returns an empty collection.
func NewCollection() *Collection {
	return &Collection{byName: map[string]*Migration{}}
}

// Add registers a migration. The name orders migrations lexically and is
// recorded in the database, so give it a sortable timestamp prefix:
//
//	c.Add("20260708093000_create_users", func(s *migrate.Schema) { ... })
//
// Add panics on an empty, oversized or duplicate name or a nil function:
// registration happens at init time, where a broken migration set should
// stop the program.
func (c *Collection) Add(name string, up func(*Schema), opts ...MigrationOption) {
	c.add(name, up, false, opts)
}

// AddRepeatable registers a repeatable migration: instead of running once, it
// runs whenever its declaration compiles to different SQL than last recorded
// — after all versioned migrations, in name order. Views, stored functions,
// triggers and reference data live here, declared idempotently
// (CREATE OR REPLACE ...), so editing the declaration in place is the whole
// workflow:
//
//	c.AddRepeatable("active_users_view", func(s *migrate.Schema) {
//		s.Exec(`CREATE OR REPLACE VIEW active_users AS SELECT ...`)
//	})
//
// Rollback never touches repeatable migrations (there is nothing to return
// to); Reset forgets their records so the next Up runs them again. A Run
// function's body is invisible to the checksum, so editing only Go logic does
// not trigger a re-run. Declaring WithDown panics — a repeatable migration
// has no down.
func (c *Collection) AddRepeatable(name string, run func(*Schema), opts ...MigrationOption) {
	c.add(name, run, true, opts)
}

func (c *Collection) add(name string, up func(*Schema), repeatable bool, opts []MigrationOption) {
	if name == "" {
		panic("migrate: migration name must not be empty")
	}
	if len(name) > 191 {
		panic(fmt.Sprintf("migrate: migration name %q exceeds 191 characters", name))
	}
	if strings.TrimSpace(name) != name {
		panic(fmt.Sprintf("migrate: migration name %q has leading or trailing whitespace", name))
	}
	if up == nil {
		panic(fmt.Sprintf("migrate: migration %q has a nil up function", name))
	}
	m := &Migration{name: name, up: up, useTx: true, repeatable: repeatable}
	for _, opt := range opts {
		opt(m)
	}
	if m.repeatable && m.down != nil {
		panic(fmt.Sprintf("migrate: repeatable migration %q cannot declare WithDown", name))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.byName[name]; dup {
		panic(fmt.Sprintf("migrate: migration %q registered twice", name))
	}
	c.byName[name] = m
}

// get returns the migration registered under name, or nil.
func (c *Collection) get(name string) *Migration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byName[name]
}

// sorted returns the versioned migrations in name order.
func (c *Collection) sorted() []*Migration {
	return c.list(false)
}

// repeatables returns the repeatable migrations in name order.
func (c *Collection) repeatables() []*Migration {
	return c.list(true)
}

func (c *Collection) list(repeatable bool) []*Migration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ms []*Migration
	for _, m := range c.byName {
		if m.repeatable == repeatable {
			ms = append(ms, m)
		}
	}
	slices.SortFunc(ms, func(a, b *Migration) int { return strings.Compare(a.name, b.name) })
	return ms
}

var defaultCollection = NewCollection()

// Add registers a migration in the default collection, the usual form inside
// a migrations package where each file registers itself:
//
//	func init() {
//		migrate.Add("20260708093000_create_users", func(s *migrate.Schema) {
//			s.Create("users", func(t *migrate.Table) {
//				t.ID()
//				t.String("email").Unique()
//				t.Timestamps()
//			})
//		})
//	}
//
// See Collection.Add for naming rules and panics.
func Add(name string, up func(*Schema), opts ...MigrationOption) {
	defaultCollection.Add(name, up, opts...)
}

// AddRepeatable registers a repeatable migration in the default collection.
// See Collection.AddRepeatable for semantics.
func AddRepeatable(name string, run func(*Schema), opts ...MigrationOption) {
	defaultCollection.AddRepeatable(name, run, opts...)
}
