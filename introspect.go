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
	Batch     int       // batch it was applied in; 0 also marks baselined rows
	AppliedAt time.Time // zero when not applied
	// Registered is false when the database has a record but the collection
	// has no migration by that name — usually a deleted migration file.
	Registered bool
	// Drifted reports that the registered declaration no longer compiles to
	// the SQL recorded when it was applied.
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
	for _, mig := range m.cfg.collection.sorted() {
		st := Status{Name: mig.name, Registered: true}
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
		st := Status{Name: r.version, Applied: true, Batch: r.batch}
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
}

// Plan renders the SQL that Up would execute, without executing anything:
// review it, hand it to a DBA, or diff it in CI. Reading the applied set is
// the only database access.
func (m *Migrator) Plan(ctx context.Context) ([]Planned, error) {
	recs, err := m.loadState(ctx, m.db)
	if err != nil {
		return nil, err
	}
	applied := make(map[string]bool, len(recs))
	for _, r := range recs {
		applied[r.version] = true
	}
	var out []Planned
	for _, mig := range m.cfg.collection.sorted() {
		if applied[mig.name] {
			continue
		}
		stmts, err := mig.compile(m.d, true)
		if err != nil {
			return nil, err
		}
		out = append(out, Planned{Name: mig.name, Statements: renderStatements(stmts)})
	}
	return out, nil
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
// renders everything, in order.
func (c *Collection) SQL(dialect Dialect) ([]Planned, error) {
	if dialect == nil {
		return nil, errors.New("migrate: dialect must not be nil")
	}
	var out []Planned
	for _, mig := range c.sorted() {
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
// no argument every registered migration is baselined; with a name, only
// migrations up to and including it. Baselined rows carry batch 0, so the
// first real Rollback never touches them (Reset still does).
func (m *Migrator) Baseline(ctx context.Context, upTo ...string) error {
	limit := optional(upTo, "")
	if limit != "" && m.cfg.collection.get(limit) == nil {
		return fmt.Errorf("migrate: baseline target %q is not a registered migration", limit)
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
		for _, mig := range m.cfg.collection.sorted() {
			if limit != "" && mig.name > limit {
				break
			}
			if applied[mig.name] {
				continue
			}
			bookkeep, err := m.bookkeepStatement(mig, 0, true)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, bookkeep.sql, bookkeep.args...); err != nil {
				return fmt.Errorf("migrate: baseline %q: %w", mig.name, err)
			}
			count++
		}
		m.cfg.logger.Info("migrate: baselined", "migrations", count)
		return nil
	})
}

// Repair re-records the checksum of every applied migration to its current
// value, accepting drift after a reviewed change — most commonly an upgrade
// of this package that renders SQL differently.
func (m *Migrator) Repair(ctx context.Context) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		recs, err := m.loadState(ctx, conn)
		if err != nil {
			return err
		}
		table := m.d.quoteIdent(m.cfg.table)
		count := 0
		for _, r := range recs {
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
