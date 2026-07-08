package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Status describes one migration: registered in the collection, applied in
// the database, or both.
type Status struct {
	Name string
	// Applied reports whether the database has a record of the migration.
	Applied   bool
	Batch     int       // batch it was applied in; 0 also marks baselined rows, -1 repeatable ones
	AppliedAt time.Time // zero when not applied
	// Registered is false when the database has a record but the collection
	// has no migration by that name — usually a deleted migration file.
	Registered bool
	// Repeatable marks migrations registered with AddRepeatable.
	Repeatable bool
	// Drifted reports that the registered declaration no longer compiles to
	// the SQL recorded when it was applied. For a repeatable migration that
	// simply means the next Up re-runs it.
	Drifted bool
}

// Status returns the merged view of registered and applied migrations in name
// order. It reads without taking the migration lock.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	recs, err := m.loadState(ctx, m.db)
	if err != nil {
		return nil, err
	}
	byVersion := make(map[string]record, len(recs))
	for _, r := range recs {
		byVersion[r.version] = r
	}

	var out []Status
	seen := make(map[string]bool)
	registered := m.cfg.collection.sorted()
	registered = append(registered, m.cfg.collection.repeatables()...)
	for _, mig := range registered {
		st := Status{Name: mig.name, Registered: true, Repeatable: mig.repeatable}
		if r, ok := byVersion[mig.name]; ok {
			st.Applied = true
			st.Batch = r.batch
			st.AppliedAt, _ = time.Parse(appliedAtFormat, r.appliedAt)
			if sum := strings.TrimSpace(r.checksum); sum != "" {
				if cur, err := mig.checksum(m.d); err == nil && cur != sum {
					st.Drifted = true
				}
			}
		}
		seen[mig.name] = true
		out = append(out, st)
	}
	for _, r := range recs {
		if seen[r.version] {
			continue
		}
		st := Status{Name: r.version, Applied: true, Batch: r.batch, Repeatable: r.batch == repeatableBatch}
		st.AppliedAt, _ = time.Parse(appliedAtFormat, r.appliedAt)
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b Status) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Planned is one migration as a dry run would execute it.
type Planned struct {
	Name string
	// Statements holds the SQL in execution order. A Run function appears as
	// a comment placeholder — its effect cannot be rendered.
	Statements []string
	// Warnings holds the safety findings for this migration (empty for
	// migrations marked Assured or when safety is off).
	Warnings []string
}

// Plan renders the SQL that Up would execute, without executing anything:
// review it, hand it to a DBA, or diff it in CI. Pending versioned migrations
// come first, then the repeatable migrations due for a re-run. Reading the
// applied set is the only database access.
func (m *Migrator) Plan(ctx context.Context) ([]Planned, error) {
	recs, err := m.loadState(ctx, m.db)
	if err != nil {
		return nil, err
	}
	applied := make(map[string]bool, len(recs))
	recorded := make(map[string]string)
	for _, r := range recs {
		applied[r.version] = true
		if r.batch == repeatableBatch {
			recorded[r.version] = strings.TrimSpace(r.checksum)
		}
	}
	var out []Planned
	for _, mig := range m.cfg.collection.sorted() {
		if applied[mig.name] {
			continue
		}
		p, err := m.plannedFor(mig)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	for _, mig := range m.cfg.collection.repeatables() {
		sum, err := mig.checksum(m.d)
		if err != nil {
			return nil, err
		}
		if prev, ok := recorded[mig.name]; ok && prev == sum {
			continue
		}
		p, err := m.plannedFor(mig)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (m *Migrator) plannedFor(mig *Migration) (Planned, error) {
	stmts, err := mig.compile(m.d, true)
	if err != nil {
		return Planned{}, err
	}
	p := Planned{Name: mig.name, Statements: renderStatements(stmts)}
	if m.cfg.safety != SafetyOff && !mig.assured {
		ops, err := mig.upOps()
		if err != nil {
			return Planned{}, err
		}
		p.Warnings = analyzeSafety(m.d.name(), ops)
	}
	return p, nil
}

// PlanRollback renders the SQL that Rollback with the same options would
// execute, without executing anything.
func (m *Migrator) PlanRollback(ctx context.Context, opts ...RollbackOption) ([]Planned, error) {
	var targets []*Migration
	err := func() error {
		conn, err := m.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("migrate: acquire connection: %w", err)
		}
		defer func() { _ = conn.Close() }()
		targets, err = m.rollbackTargets(ctx, conn, resolveSpec(opts))
		return err
	}()
	if err != nil {
		return nil, err
	}
	var out []Planned
	for _, mig := range targets {
		stmts, err := mig.compile(m.d, false)
		if err != nil {
			return nil, err
		}
		out = append(out, Planned{Name: mig.name, Statements: renderStatements(stmts)})
	}
	return out, nil
}

// SQL renders every migration in the collection for a dialect without
// touching any database — offline review, docs, or handing a script to a DBA.
// Unlike Migrator.Plan it does not know what is already applied, so it
// renders everything: versioned migrations in order, then repeatable ones.
func (c *Collection) SQL(dialect Dialect) ([]Planned, error) {
	if dialect == nil {
		return nil, errors.New("migrate: dialect must not be nil")
	}
	var out []Planned
	for _, mig := range append(c.sorted(), c.repeatables()...) {
		stmts, err := mig.compile(dialect, true)
		if err != nil {
			return nil, err
		}
		out = append(out, Planned{Name: mig.name, Statements: renderStatements(stmts)})
	}
	return out, nil
}

func renderStatements(stmts []statement) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		switch {
		case s.fn != nil:
			out[i] = "-- Go function: not renderable, runs at migration time"
		case len(s.args) > 0:
			out[i] = fmt.Sprintf("%s\n-- args: %v", s.sql, s.args)
		default:
			out[i] = s.sql
		}
	}
	return out
}

// Baseline records registered migrations as applied without executing them,
// for adopting this package on a database whose schema already exists. With
// no argument every registered migration is baselined; with a name, versioned
// migrations up to and including it. Repeatable migrations are always
// baselined at their current checksum. Baselined rows carry batch 0, so the
// first real Rollback never touches them (Reset still does).
func (m *Migrator) Baseline(ctx context.Context, upTo ...string) error {
	if len(upTo) > 1 {
		return fmt.Errorf("migrate: Baseline takes at most one target, got %d", len(upTo))
	}
	limit := optional(upTo, "")
	if limit != "" {
		target := m.cfg.collection.get(limit)
		if target == nil {
			return fmt.Errorf("migrate: baseline target %q is not a registered migration", limit)
		}
		if target.repeatable {
			return fmt.Errorf("migrate: baseline target %q is repeatable; the bound must be a versioned migration (repeatables are always baselined)", limit)
		}
	}
	return m.locked(ctx, func(conn *sql.Conn) error {
		recs, err := m.loadState(ctx, conn)
		if err != nil {
			return err
		}
		applied := make(map[string]bool, len(recs))
		for _, r := range recs {
			applied[r.version] = true
		}
		count := 0
		record := func(mig *Migration, batch int) error {
			if applied[mig.name] {
				return nil
			}
			bookkeep, err := m.insertRecord(mig, batch)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, bookkeep.sql, bookkeep.args...); err != nil {
				return fmt.Errorf("migrate: baseline %q: %w", mig.name, err)
			}
			count++
			return nil
		}
		for _, mig := range m.cfg.collection.sorted() {
			if limit != "" && mig.name > limit {
				break
			}
			if err := record(mig, 0); err != nil {
				return err
			}
		}
		for _, mig := range m.cfg.collection.repeatables() {
			if err := record(mig, repeatableBatch); err != nil {
				return err
			}
		}
		m.cfg.logger.Info("migrate: baselined", "migrations", count)
		return nil
	})
}

// Fresh drops every table in the database — the migration records included —
// and runs all migrations from scratch. It exists for development flow:
// when an irreversible migration blocks Rollback, Fresh starts over instead.
// It destroys all data; nothing about it belongs near production.
//
// Tables are dropped in passes until none remain, which unwinds foreign key
// dependencies without touching session state; Postgres drops CASCADE, so
// dependent objects such as views go too. Other standalone objects survive,
// and idempotent repeatable migrations recreate theirs on the way back up.
func (m *Migrator) Fresh(ctx context.Context) error {
	err := m.locked(ctx, func(conn *sql.Conn) error {
		tables, err := m.listTables(ctx, conn)
		if err != nil {
			return err
		}
		dropped := 0
		for len(tables) > 0 {
			var remaining []string
			var lastErr error
			for _, table := range tables {
				if _, err := conn.ExecContext(ctx, m.d.freshDropSQL(table)); err != nil {
					lastErr = err
					remaining = append(remaining, table)
					continue
				}
				dropped++
			}
			if len(remaining) == len(tables) {
				return fmt.Errorf("migrate: fresh: drop tables: %w", lastErr)
			}
			tables = remaining
		}
		m.cfg.logger.Info("migrate: fresh dropped all tables", "tables", dropped)
		return nil
	})
	if err != nil {
		return err
	}
	return m.Up(ctx)
}

func (m *Migrator) listTables(ctx context.Context, db DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, m.d.listTablesSQL())
	if err != nil {
		return nil, fmt.Errorf("migrate: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("migrate: list tables: %w", err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: list tables: %w", err)
	}
	return tables, nil
}

// Repair re-records the checksum of every applied versioned migration to its
// current value, accepting drift after a reviewed change — most commonly an
// upgrade of this package that renders SQL differently. Repeatable records
// are left alone: for those a changed checksum means a pending re-run, and
// rewriting it would silently cancel that re-run.
func (m *Migrator) Repair(ctx context.Context) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		recs, err := m.loadState(ctx, conn)
		if err != nil {
			return err
		}
		table := m.d.quoteIdent(m.cfg.table)
		count := 0
		for _, r := range recs {
			if r.batch == repeatableBatch {
				continue
			}
			mig := m.cfg.collection.get(r.version)
			if mig == nil {
				continue
			}
			sum, err := mig.checksum(m.d)
			if err != nil {
				return err
			}
			if sum == strings.TrimSpace(r.checksum) {
				continue
			}
			query := fmt.Sprintf("UPDATE %s SET checksum = %s WHERE version = %s",
				table, m.d.placeholder(1), m.d.placeholder(2))
			if _, err := conn.ExecContext(ctx, query, sum, r.version); err != nil {
				return fmt.Errorf("migrate: repair %q: %w", r.version, err)
			}
			count++
		}
		m.cfg.logger.Info("migrate: repaired checksums", "migrations", count)
		return nil
	})
}
