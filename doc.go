// Package migrate runs database schema migrations that are written as Go
// code and compiled into the binary — no SQL files to ship, no external
// tools.
//
// A migration declares its changes on a fluent schema builder:
//
//	func init() {
//		migrate.Add("20260708093000_create_users", func(s *migrate.Schema) {
//			s.Create("users", func(t *migrate.Table) {
//				t.ID()
//				t.String("email").Unique()
//				t.String("name")
//				t.ForeignID("team_id").Constrained().CascadeOnDelete()
//				t.Timestamps()
//			})
//		})
//	}
//
// and the application applies whatever is pending at a moment of its
// choosing, typically startup or a deploy job:
//
//	db, err := sql.Open("pgx", dsn)
//	...
//	m, err := migrate.New(db, migrate.Postgres)
//	...
//	if err := m.Up(ctx); err != nil {
//		log.Fatal(err)
//	}
//
// The declaration is data, not executed SQL, which is what the rest of the
// package is built on: the same declaration compiles to the configured
// dialect (Postgres, MySQL or SQLite), reverses itself for Rollback without a
// hand-written down migration, renders as reviewable SQL in Plan before
// anything runs, and hashes into a checksum that detects migrations edited
// after they were applied.
//
// Operations that discard information — dropping tables or columns, raw Exec,
// Run functions — cannot be reversed automatically; rolling them back
// requires an explicit down declared with WithDown, and is otherwise refused
// with ErrIrreversible.
//
// Migrations registered with AddRepeatable run whenever their declaration
// changes rather than once: views, stored functions and reference data are
// edited in place, and the next Up re-runs them after all versioned
// migrations.
//
// Concurrent migrators (replicas racing at deploy time) are serialized with a
// session-level advisory lock on Postgres and MySQL, which the database
// releases automatically if a migrator crashes; on SQLite the single-writer
// file arbitrates instead, and a racer that loses fails cleanly on the
// records table with guidance to rerun. A failed migration rolls back
// with its transaction on Postgres and SQLite and is never half-recorded;
// there is no "dirty" state to clear by hand. MySQL commits DDL implicitly
// and cannot offer that atomicity — failures there report exactly which
// statement failed and what was already committed.
//
// The package depends only on the standard library: open *sql.DB with
// whatever driver you already use and pass it in.
package migrate
