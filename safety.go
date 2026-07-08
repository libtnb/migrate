package migrate

import (
	"errors"
	"fmt"
)

// ErrUnsafe marks an Up refused under SafetyStrict because pending migrations
// contain operations that lock or break a live production system. Each
// violation carries advice; Assured() acknowledges a reviewed migration.
var ErrUnsafe = errors.New("migrate: unsafe migration")

// SafetyLevel selects what happens when a pending migration contains an
// operation that is dangerous on a live system — locking a large table,
// breaking code that is still deployed.
type SafetyLevel int

const (
	// SafetyWarn, the default, logs each finding through the configured
	// logger and proceeds.
	SafetyWarn SafetyLevel = iota
	// SafetyStrict refuses to run: Up fails with ErrUnsafe before executing
	// anything, listing every finding. Meant for CI.
	SafetyStrict
	// SafetyOff disables the analysis.
	SafetyOff
)

// WithSafety sets the safety level. The default is SafetyWarn.
func WithSafety(level SafetyLevel) Option {
	return func(cfg *config) { cfg.safety = level }
}

// Assured marks a migration as reviewed: the safety analysis skips it. Use it
// to acknowledge a finding after deciding the operation is fine — the table
// is small, the app no longer reads the column, the maintenance window is
// open.
func Assured() MigrationOption {
	return func(m *Migration) { m.assured = true }
}

// analyzeSafety inspects recorded operations for patterns that are safe on an
// empty development database and hazardous on a loaded production one. Only
// alterations and drops are flagged — creating a table is always safe, and
// raw SQL and Go functions are deliberately not second-guessed.
func analyzeSafety(dialect string, ops []operation) []string {
	var findings []string
	warn := func(format string, a ...any) {
		findings = append(findings, fmt.Sprintf(format, a...))
	}
	for _, op := range ops {
		switch o := op.(type) {
		case *recreateTable:
			warn("recreating table %q copies every row while holding locks; plan for the copy time on large tables, and note that a table referenced by foreign keys or views cannot be recreated on Postgres", o.def.name)
		case *dropTable:
			warn("dropping table %q breaks application code still using it; deploy code that stopped using it first", o.name)
		case *renameTable:
			warn("renaming table %q to %q is not backward compatible with running code; prefer creating the new table, migrating data and dropping the old one across deploys", o.from, o.to)
		case *alterTable:
			for _, ch := range o.changes {
				switch c := ch.(type) {
				case *addColumn:
					if !c.col.nullable && !c.col.hasDefault && !c.col.useCurrent && !c.col.autoIncr {
						warn("adding NOT NULL column %q to existing table %q fails when rows exist; add a Default, or make it Nullable and backfill", c.col.name, o.table)
					}
				case *dropColumn:
					warn("dropping column %q of table %q breaks application code still reading it; deploy code that stopped using it first", c.name, o.table)
				case *renameColumn:
					warn("renaming column %q of table %q to %q is not backward compatible with running code; prefer adding the new column, dual-writing and dropping the old one across deploys", c.from, o.table, c.to)
				case *addIndex:
					if dialect == "postgres" {
						warn("adding index %q blocks writes to %q while it builds; on a large table use CREATE INDEX CONCURRENTLY via Exec on a WithoutTransaction migration", c.idx.resolvedName(o.table), o.table)
					}
				case *addForeign:
					if dialect == "postgres" {
						warn("adding foreign key %q validates every row of %q under lock; on a large table add it NOT VALID via Exec, then VALIDATE CONSTRAINT separately", c.fk.resolvedName(o.table), o.table)
					}
				case *addPrimary:
					warn("adding a primary key to existing table %q rewrites the table under lock on most engines", o.table)
				case *addCheck:
					if dialect == "postgres" {
						warn("adding check constraint %q validates every row of %q under lock; on a large table add it NOT VALID via Exec, then VALIDATE CONSTRAINT separately", c.chk.name, o.table)
					}
				}
			}
		}
	}
	return findings
}

// checkSafety applies the configured level to the migrations about to run.
// Under SafetyStrict every finding across all migrations is collected first,
// so nothing executes and the error reports the full picture.
func (m *Migrator) checkSafety(migs []*Migration) error {
	if m.cfg.safety == SafetyOff {
		return nil
	}
	var violations []string
	for _, mig := range migs {
		if mig.assured {
			continue
		}
		ops, err := mig.upOps()
		if err != nil {
			return err
		}
		for _, finding := range analyzeSafety(m.d.name(), ops) {
			if m.cfg.safety == SafetyStrict {
				violations = append(violations, fmt.Sprintf("%s: %s", mig.name, finding))
			} else {
				m.cfg.logger.Warn("migrate: safety", "migration", mig.name, "finding", finding)
			}
		}
	}
	if len(violations) == 0 {
		return nil
	}
	msg := ""
	for _, v := range violations {
		msg += "\n  - " + v
	}
	return fmt.Errorf("%w:%s\n(review each finding, then mark the migration Assured() or lower the level with WithSafety)", ErrUnsafe, msg)
}
