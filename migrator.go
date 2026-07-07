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

// appliedAtFormat stores applied-at instants as sortable UTC strings, which
// every dialect round-trips identically with no driver time configuration.
const appliedAtFormat = "2006-01-02T15:04:05.000000Z"

// Migrator applies and rolls back the migrations of one collection against
// one database. Methods are safe to call from concurrent processes: runs are
// serialized by a database advisory lock (see WithoutLock to opt out).
type Migrator struct {
	db  *sql.DB
	d   Dialect
	cfg config
}

// New creates a Migrator for db, which stays owned by the caller. The dialect
// must match the driver the caller opened db with — this package imports no
// drivers:
//
//	db, _ := sql.Open("pgx", dsn)
//	m, err := migrate.New(db, migrate.Postgres)
func New(db *sql.DB, dialect Dialect, opts ...Option) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("migrate: db must not be nil")
	}
	if dialect == nil {
		return nil, errors.New("migrate: dialect must not be nil")
	}
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Migrator{db: db, d: dialect, cfg: cfg}, nil
}

// record is one row of the records table.
type record struct {
	version   string
	batch     int
	checksum  string
	appliedAt string
}

// Up applies every registered migration that has not been applied yet, in
// name order, as one batch. Each migration runs in its own transaction unless
// it opted out; a failure stops the run, leaves no partial bookkeeping and
// returns an error identifying the failing statement.
func (m *Migrator) Up(ctx context.Context) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		recs, err := m.loadState(ctx, conn)
		if err != nil {
			return err
		}
		if err := m.verifyChecksums(recs); err != nil {
			return err
		}

		applied := make(map[string]bool, len(recs))
		batch := 0
		for _, r := range recs {
			applied[r.version] = true
			batch = max(batch, r.batch)
		}
		batch++

		pending := 0
		for _, mig := range m.cfg.collection.sorted() {
			if applied[mig.name] {
				continue
			}
			if err := m.runOne(ctx, conn, mig, batch, true); err != nil {
				return err
			}
			pending++
		}
		if pending == 0 {
			m.cfg.logger.Info("migrate: nothing to apply")
		}
		return nil
	})
}

// RollbackOption narrows what Rollback and PlanRollback roll back.
type RollbackOption func(*rollbackSpec)

type rollbackSpec struct {
	steps int // 0 means the whole latest batch
}

// Steps limits the rollback to the n most recently applied migrations,
// regardless of batches.
func Steps(n int) RollbackOption {
	return func(s *rollbackSpec) { s.steps = n }
}

// Rollback reverses the most recent batch of migrations (or the given Steps)
// in reverse application order. Migrations declared with WithDown run their
// explicit down; the rest reverse their recorded operations automatically. A
// migration whose operations discard information (Drop, DropColumn, Exec,
// Run) fails with ErrIrreversible instead of guessing.
func (m *Migrator) Rollback(ctx context.Context, opts ...RollbackOption) error {
	return m.rollback(ctx, resolveSpec(opts))
}

// Reset rolls back everything that was ever applied, leaving an empty
// database. Every applied migration must be reversible or declare a down.
func (m *Migrator) Reset(ctx context.Context) error {
	return m.rollback(ctx, rollbackSpec{steps: -1})
}

func resolveSpec(opts []RollbackOption) rollbackSpec {
	var spec rollbackSpec
	for _, opt := range opts {
		opt(&spec)
	}
	return spec
}

func (m *Migrator) rollback(ctx context.Context, spec rollbackSpec) error {
	return m.locked(ctx, func(conn *sql.Conn) error {
		targets, err := m.rollbackTargets(ctx, conn, spec)
		if err != nil {
			return err
		}
		if len(targets) == 0 {
			m.cfg.logger.Info("migrate: nothing to roll back")
			return nil
		}
		for _, mig := range targets {
			if err := m.runOne(ctx, conn, mig, 0, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// rollbackTargets resolves which applied migrations the spec selects, newest
// first, failing when one of them is not registered in the collection.
func (m *Migrator) rollbackTargets(ctx context.Context, conn *sql.Conn, spec rollbackSpec) ([]*Migration, error) {
	recs, err := m.loadState(ctx, conn)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, nil
	}
	// Reverse application order: later batches first, later names first
	// within a batch.
	slices.SortFunc(recs, func(a, b record) int {
		if a.batch != b.batch {
			return b.batch - a.batch
		}
		return strings.Compare(b.version, a.version)
	})
	switch {
	case spec.steps < 0: // Reset
	case spec.steps > 0:
		recs = recs[:min(spec.steps, len(recs))]
	default:
		latest := recs[0].batch
		for i, r := range recs {
			if r.batch != latest {
				recs = recs[:i]
				break
			}
		}
	}
	targets := make([]*Migration, len(recs))
	for i, r := range recs {
		mig := m.cfg.collection.get(r.version)
		if mig == nil {
			return nil, fmt.Errorf("migrate: cannot roll back %q: not registered in this build (was the migration file deleted?)", r.version)
		}
		targets[i] = mig
	}
	return targets, nil
}

// runOne executes one migration in the requested direction and records or
// removes its row, atomically with the migration itself when transactions
// are in play.
func (m *Migrator) runOne(ctx context.Context, conn *sql.Conn, mig *Migration, batch int, up bool) error {
	verb, verbed := "apply", "applied"
	if !up {
		verb, verbed = "roll back", "rolled back"
	}
	stmts, err := mig.compile(m.d, up)
	if err != nil {
		return err
	}
	bookkeep, err := m.bookkeepStatement(mig, batch, up)
	if err != nil {
		return err
	}
	stmts = append(stmts, bookkeep)

	start := time.Now()
	if mig.useTx {
		err = m.runInTx(ctx, conn, stmts)
	} else {
		err = runStatements(ctx, conn, stmts)
	}
	if err != nil {
		return fmt.Errorf("migrate: %s %q: %w", verb, mig.name, err)
	}
	m.cfg.logger.Info("migrate: "+verbed, "migration", mig.name, "duration", time.Since(start).Round(time.Millisecond))
	return nil
}

func (m *Migrator) runInTx(ctx context.Context, conn *sql.Conn, stmts []statement) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := runStatements(ctx, tx, stmts); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			m.cfg.logger.Warn("migrate: rollback after failure", "error", rbErr)
		}
		if !m.d.transactionalDDL() {
			err = fmt.Errorf("%w (%s DDL commits implicitly: earlier DDL statements of this migration are already in effect; reconcile the schema before retrying)", err, m.d.name())
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func runStatements(ctx context.Context, db DB, stmts []statement) error {
	for i, s := range stmts {
		var err error
		if s.fn != nil {
			err = s.fn(ctx, db)
		} else if _, execErr := db.ExecContext(ctx, s.sql, s.args...); execErr != nil {
			err = execErr
		}
		if err != nil {
			return fmt.Errorf("statement %d/%d (%s): %w", i+1, len(stmts), describeStatement(s), err)
		}
	}
	return nil
}

func describeStatement(s statement) string {
	if s.fn != nil {
		return "Go function"
	}
	sql := strings.Join(strings.Fields(s.sql), " ")
	if len(sql) > 200 {
		sql = sql[:200] + "…"
	}
	return sql
}

// bookkeepStatement returns the records-table mutation that makes the
// migration count as applied (or no longer applied).
func (m *Migrator) bookkeepStatement(mig *Migration, batch int, up bool) (statement, error) {
	table := m.d.quoteIdent(m.cfg.table)
	if !up {
		return statement{
			sql:  fmt.Sprintf("DELETE FROM %s WHERE version = %s", table, m.d.placeholder(1)),
			args: []any{mig.name},
		}, nil
	}
	sum, err := mig.checksum(m.d)
	if err != nil {
		return statement{}, err
	}
	return statement{
		sql: fmt.Sprintf("INSERT INTO %s (version, batch, checksum, applied_at) VALUES (%s, %s, %s, %s)",
			table, m.d.placeholder(1), m.d.placeholder(2), m.d.placeholder(3), m.d.placeholder(4)),
		args: []any{mig.name, batch, sum, m.cfg.clock.Now().UTC().Format(appliedAtFormat)},
	}, nil
}

// loadState ensures the records table exists and returns its rows in name
// order.
func (m *Migrator) loadState(ctx context.Context, db DB) ([]record, error) {
	if _, err := db.ExecContext(ctx, m.d.ensureTableSQL(m.cfg.table)); err != nil {
		return nil, fmt.Errorf("migrate: create records table: %w", err)
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		"SELECT version, batch, checksum, applied_at FROM %s ORDER BY version", m.d.quoteIdent(m.cfg.table)))
	if err != nil {
		return nil, fmt.Errorf("migrate: read records table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var recs []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.version, &r.batch, &r.checksum, &r.appliedAt); err != nil {
			return nil, fmt.Errorf("migrate: read records table: %w", err)
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read records table: %w", err)
	}
	return recs, nil
}

// verifyChecksums compares each applied migration against what its current
// declaration compiles to. A mismatch means the migration changed after it
// ran — a warning by default, an error under WithStrictChecksum. Records
// without a checksum (from Baseline of older tooling) are skipped.
func (m *Migrator) verifyChecksums(recs []record) error {
	for _, r := range recs {
		if strings.TrimSpace(r.checksum) == "" {
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
		if sum != strings.TrimSpace(r.checksum) {
			if m.cfg.strictChecksum {
				return fmt.Errorf("%w: %q no longer compiles to the SQL it was applied with (run Repair to accept the current form)", ErrChecksumMismatch, r.version)
			}
			m.cfg.logger.Warn("migrate: checksum mismatch — the migration changed after it was applied",
				"migration", r.version)
		}
	}
	return nil
}

// locked runs fn on a dedicated connection while holding the advisory lock.
// Session-level locks die with the connection, so a crashed migrator never
// wedges the next one.
func (m *Migrator) locked(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if m.cfg.lock {
		if err := m.d.lock(ctx, conn, m.cfg.table, m.cfg.lockTimeout); err != nil {
			return err
		}
		defer func() {
			if err := m.d.unlock(context.WithoutCancel(ctx), conn, m.cfg.table); err != nil {
				m.cfg.logger.Warn("migrate: release lock", "error", err)
			}
		}()
	}
	return fn(conn)
}
