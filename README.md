# migrate

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/migrate.svg)](https://pkg.go.dev/github.com/go-rio/migrate)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/migrate)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/migrate.svg)](https://github.com/go-rio/migrate/releases)
[![Test](https://github.com/go-rio/migrate/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/migrate/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/go-rio/migrate)](https://opensource.org/license/MIT)

Database schema migrations written as Go code and compiled into your binary: no SQL files to ship, no CLI to install, no third-party dependencies.

A migration declares its changes once on a fluent schema builder. One declaration produces:

- Dialect-specific SQL for PostgreSQL, MySQL, and SQLite.
- Automatic rollback with no hand-written down migration.
- A reviewable dry-run plan.
- A checksum that detects migrations edited after they ran.

```go
func init() {
	migrate.Add("20260708100000_create_users", func(s *migrate.Schema) {
		s.Create("users", func(t *migrate.Table) {
			t.ID()
			t.String("email").Unique()
			t.String("name", 100)
			t.Enum("role", "admin", "member").Default("member")
			t.ForeignID("team_id").Constrained().CascadeOnDelete()
			t.Timestamps()
		})
	})
}
```

```go
db, err := sql.Open("pgx", dsn) // any database/sql driver you already use
...
m, err := migrate.New(db, migrate.Postgres)
...
if err := m.Up(ctx); err != nil {
	log.Fatal(err)
}
```

## Features

| Feature | Behavior |
|---|---|
| Migrations are code | One Go file per migration, self-registered in `init`; `go build` packages the full history into the binary. A broken migration is a compile error. |
| Automatic rollbacks | `Rollback` reverses recorded operations in reverse order (e.g. an added index drops before its column). Information-discarding operations — dropping tables/columns, raw SQL, Go functions — need an explicit `WithDown`, else fail with `ErrIrreversible`. |
| No dirty state | The migration row is written atomically with the migration; on Postgres and SQLite a failure rolls it fully back and the next `Up` retries. No `force` flag. MySQL commits DDL implicitly, so failures name the failing statement and those already committed, or report a clean rollback when no DDL ran. |
| Concurrency-safe | A session-level advisory lock on a dedicated connection serializes racing replicas and is released by the database on crash. SQLite has none: each migration's first write is its own records row, so a losing racer fails on the records table before touching the schema, with rerun guidance. Opt out with `WithoutLock`. |
| Tamper detection | Each applied migration records a checksum of its compiled SQL. On drift, `Up` warns (or fails under `WithStrictChecksum`), `Status` reports it, and `Repair` re-records after review. |
| Repeatable migrations | `AddRepeatable` re-runs views, functions, triggers, and reference data when their declaration changes (see below). |
| Reviewable | `Plan` renders pending SQL against a live database; `Collection.SQL` renders a collection offline with no database; `PlanRollback` previews a rollback. |
| Dialect types | Compiles to each engine's best-practice types: `JSONB`, `TIMESTAMPTZ`, identity columns on Postgres; `DATETIME(6)`, native `ENUM` on MySQL. Unsupported operations fail at compile time, not silently. |
| Batch history | Each `Up` is one batch; `Rollback`, `RollbackBatch`, `Reset`, and `Baseline` manage history (see below). |
| Zero dependencies | Only `database/sql` and the standard library; pass in your own `*sql.DB` and driver. |

## Installation

```bash
go get github.com/go-rio/migrate
```

> Moved from `github.com/libtnb/migrate` in v0.5.0 — part of the
> [go-rio](https://github.com/go-rio) family alongside the
> [rio](https://github.com/go-rio/rio) ORM. Releases up to v0.4.0 remain
> installable from the old path; new releases ship here only. The advisory
> lock namespace was renamed with the module, so avoid running migrations
> concurrently from pre-v0.5.0 and post-v0.5.0 binaries during a rolling
> upgrade.

## Writing migrations

Conventional layout: a `migrations` package, one file per migration, imported for effect from `main`.

```
app/
├── main.go
└── migrations/
    ├── 20260708100000_create_users.go
    ├── 20260712093000_create_posts.go
    └── 20260801154500_backfill_display_names.go
```

```go
// main.go
import _ "app/migrations"
```

Names order lexically and are recorded in the database, so start them with a sortable timestamp. Registration panics on duplicate or malformed names at init time.

### Columns

```go
s.Create("articles", func(t *migrate.Table) {
	t.ID()                              // auto-incrementing 64-bit primary key
	                                    // (t.Integer("id").AutoIncrement() for other widths)
	t.String("slug", 80).Unique()       // VARCHAR(80) + unique index
	t.Text("body")                      // unbounded text
	t.Integer("views").Default(0)
	t.Decimal("rating", 3, 1).Nullable()
	t.Boolean("published").Default(false)
	t.JSON("meta").Nullable()           // JSONB / JSON / TEXT
	t.UUID("public_id").DefaultExpr("gen_random_uuid()")
	t.Enum("state", "draft", "live")    // native ENUM or CHECK constraint
	t.TimestampTz("published_at").Nullable()
	t.Timestamps()                      // created_at, updated_at
	t.SoftDeletes()                     // deleted_at
	t.Column("tags", "text[]")          // any dialect type, verbatim
})
```

Columns are `NOT NULL` unless declared `.Nullable()`. Modifiers chain:

- Any engine: `.Default(v)`, `.DefaultExpr(expr)`, `.UseCurrent()`, `.Unsigned()`, `.Unique()`, `.Index()`, `.Primary()`, `.AutoIncrement()`, `.Comment(...)`.
- Generated columns: `.StoredAs(expr)`, `.VirtualAs(expr)`.
- MySQL only: `.After(...)`, `.First()`, `.UseCurrentOnUpdate()`.

Table names may be schema-qualified: `s.Create("analytics.events", ...)` renders `"analytics"."events"`, with conventional constraint names inside the schema.

CHECK constraints must be named; anonymous checks are rejected (an unnamed constraint cannot be dropped later).

```go
t.Check("orders_price_positive", "price > 0")   // in Create or Table
t.DropCheck("orders_price_positive")            // reverses an added check
```

`.AutoIncrement()` makes any integer column the database-generated primary key, in each engine's form:

| Engine | Form |
|---|---|
| Postgres | identity column, `GENERATED BY DEFAULT AS IDENTITY` (not legacy serial) |
| MySQL | `AUTO_INCREMENT` |
| SQLite | `INTEGER PRIMARY KEY AUTOINCREMENT` |

`t.ID()` is shorthand for `t.BigInteger("id").Unsigned().AutoIncrement()`.

### Indexes and foreign keys

```go
t.Index("a", "b")                 // articles_a_b_index
t.Unique("slug").Name("custom")   // custom name
t.Primary("a", "b")               // composite primary key

t.ForeignID("user_id").Constrained()              // → users.id, inferred
t.ForeignID("category_id").Constrained().NullOnDelete()
t.Foreign("code").References("regions", "code")   // existing column, explicit
```

Names follow `{table}_{columns}_{index|unique|foreign}`, so dropping by columns (`t.DropIndex("a", "b")`, `t.DropForeign("user_id")`) reconstructs the created name.

### Altering tables

```go
migrate.Add("20260801120000_polish_users", func(s *migrate.Schema) {
	s.Table("users", func(t *migrate.Table) {
		t.String("nickname", 50).Nullable().Index()
		t.RenameColumn("name", "full_name")
		t.DropColumn("legacy_flags")
	})
	s.Rename("groups", "teams")
})
```

Each change compiles to its own statement and reverses individually; the migration runs in one transaction where the engine allows. Also: `t.RenameIndex(from, to)`, `t.DropIndex`/`DropUnique`/`DropForeign` (by columns, via conventional names), `t.DropPrimary()`, and table comments via `t.Comment(...)`.

### Rebuilding tables

`Recreate` handles what `ALTER TABLE` cannot (on SQLite, any constraint change): declare the full target table and the migrator rebuilds it around the data (create temporary → copy rows → capture triggers → drop old → rename → rebuild indexes → recreate triggers), within the migration's transaction on Postgres and SQLite, so a failure leaves the original untouched. MySQL refuses to compile `Recreate`: its implicit DDL commits would open a crash window with the live table dropped, and its native `ALTER TABLE` changes types and constraints directly.

```go
s.Recreate("users", func(t *migrate.Table) {
	t.ID()
	t.String("email").Unique()                                 // the new constraint
	t.Integer("logins").Default(0).SkipCopy()                  // brand-new column
})
```

- `Recreate` requires its transaction; combining it with `WithoutTransaction` is a compile-time error.
- On Postgres, a table referenced by other tables' foreign keys or by views cannot be rebuilt; the drop is refused and the transaction rolls back cleanly. Use native `ALTER` there.

Columns copy by name. `SkipCopy` marks columns absent from the old table; `CopyFrom` substitutes a SELECT expression, renaming and retyping a column in one rebuild:

```go
t.Integer("age").CopyFrom("CAST(age AS INTEGER)")
```

Conventional constraint and index names come out for the *final* table name, so later `DropUnique`/`DropForeign` still resolve. `Recreate` discards the old definition, so rolling back needs a `WithDown` (usually another `Recreate` for the previous shape).

SQLite caveat: with `PRAGMA foreign_keys=ON` and child rows referencing the table, run on a connection with enforcement off (off by default in SQLite and most drivers).

Triggers (created via `Exec`, since the builder does not declare them) are captured and recreated verbatim after the rename, which `DROP TABLE` would otherwise remove. A trigger the new shape breaks fails the replay and rolls back; drop it with `Exec` before the `Recreate` and declare its successor after.

There is no `Change()` for redefining a column type in place; SQLite cannot without rebuilding. Use SQL — reviewable in `Plan` and checksummed:

```go
migrate.Add("20260812100000_widen_amounts",
	func(s *migrate.Schema) {
		s.Exec(`ALTER TABLE orders ALTER COLUMN amount TYPE NUMERIC(12, 2)`)
	},
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`ALTER TABLE orders ALTER COLUMN amount TYPE NUMERIC(8, 2)`)
	}),
)
```

### Raw SQL and data migrations

```go
migrate.Add("20260805090000_backfill",
	func(s *migrate.Schema) {
		s.Exec(`UPDATE users SET plan = 'free' WHERE plan IS NULL`)
		s.Run(func(ctx context.Context, db migrate.DB) error {
			// arbitrary Go: batched backfills, API lookups, encoding changes
			_, err := db.ExecContext(ctx, `UPDATE users SET score = score * 10`)
			return err
		})
	},
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`UPDATE users SET score = score / 10`)
	}),
)
```

`Run` receives the migration's transaction when present; `migrate.DB` is satisfied by both `*sql.Tx` and `*sql.DB`. Statements that cannot run in a transaction (e.g. `CREATE INDEX CONCURRENTLY`) set `migrate.WithoutTransaction()`:

```go
migrate.Add("20260810110000_index_events",
	func(s *migrate.Schema) {
		s.Exec(`CREATE INDEX CONCURRENTLY events_at_index ON events (at)`)
	},
	migrate.WithoutTransaction(),
	migrate.WithDown(func(s *migrate.Schema) {
		s.Exec(`DROP INDEX events_at_index`)
	}),
)
```

### Repeatable migrations

A versioned migration runs once; a repeatable migration runs whenever its compiled SQL differs from the last recorded value. Use it for views, stored functions, triggers, and reference data.

```go
migrate.AddRepeatable("active_users_view", func(s *migrate.Schema) {
	s.Exec(`CREATE OR REPLACE VIEW active_users AS
	        SELECT * FROM users WHERE deleted_at IS NULL`)
})
```

- Change the declaration and deploy; the next `Up` re-runs it, after all versioned migrations, in name order.
- Declarations must be idempotent: `CREATE OR REPLACE`, or `DROP ... IF EXISTS` + `CREATE` on SQLite (no `OR REPLACE`).
- `Status` shows a changed repeatable as drifted-pending; `Plan` renders what would re-run; rollbacks leave repeatables untouched (`Reset` forgets their records, so a fresh `Up` re-runs them).
- A `Run` body is invisible to the checksum; edit SQL, not Go, to trigger a re-run.
- Postgres refuses to roll back a versioned migration whose table a live repeatable view depends on; drop the dependent object first.

## Running

| Call | Effect |
|---|---|
| `m.Up(ctx)` | apply all pending migrations as one batch |
| `m.Rollback(ctx, 1)` | undo the most recently applied migration |
| `m.Rollback(ctx, n)` | undo the n most recent migrations |
| `m.RollbackBatch(ctx)` | undo the latest batch — everything the last `Up` applied |
| `m.Reset(ctx)` | undo everything |
| `m.Status(ctx)` | applied / pending / drifted / unregistered, per migration |
| `m.Plan(ctx)` / `m.PlanRollback(ctx, n)` / `m.PlanRollbackBatch(ctx)` | the SQL that would run, without running it |
| `m.Baseline(ctx)` | mark migrations applied without executing (existing databases) |
| `m.Repair(ctx)` | re-record versioned checksums after a reviewed change (repeatables stay due) |
| `m.Fresh(ctx)` | **development only**: drop every table, re-run everything |

Run `Up` at startup (safe across replicas via the advisory lock) or from a dedicated deploy step:

```go
m, err := migrate.New(db, migrate.Postgres,
	migrate.WithLogger(slog.Default()),
	migrate.WithLockTimeout(2*time.Minute), // wait for a sibling deploy
)
```

Options: `WithCollection` (explicit collection instead of the global registry), `WithTable` (records table name), `WithoutLock`, `WithLockTimeout`, `WithStrictChecksum`, `WithLogger`, `WithClock`.

## Safety analysis

Some operations are safe on an empty database but dangerous on a loaded one: dropping a column live code still reads, adding a `NOT NULL` column to a populated table, building an index that blocks writes. The migrator detects these before executing anything.

| Mode | Behavior |
|---|---|
| `SafetyWarn` (default) | logs each finding through `WithLogger` and proceeds |
| `WithSafety(migrate.SafetyStrict)` | `Up` fails with `ErrUnsafe` before executing, listing every finding across the run; wire it into CI |
| `Assured()` | marks a reviewed migration so the analysis skips it |

`Plan` attaches findings to each planned migration. Creating tables never warns; raw `Exec`/`Run` are not analyzed. The analysis covers declarative operations: destructive drops, backward-incompatible renames, `NOT NULL` additions without defaults, and Postgres index/foreign-key builds that lock large tables (the message names the `CONCURRENTLY` / `NOT VALID` escape routes).

## Transactions and engines

| | PostgreSQL | MySQL | SQLite |
|---|---|---|---|
| DDL in transactions | yes — failed migrations roll back completely | no — DDL commits implicitly | yes — failed migrations roll back completely |
| Advisory lock | `pg_advisory_lock`, session-level | `GET_LOCK`, session-level | none — the single-writer file and record-first bookkeeping arbitrate |
| Altering constraints | full support | full support | compile-time error with guidance |

Each migration runs in its own transaction by default. On MySQL the transaction still protects data statements, but a mid-migration failure leaves earlier DDL in effect; the error says so, names the failing statement, and does not record the migration, so a fixed version runs again. Keep MySQL migrations to a single DDL statement where possible.

Session-level advisory locks do not survive transaction-pooling proxies (PgBouncer in transaction mode). Point the migrator at the database directly or through a session-mode pool.

## Design notes

- **Declarations are data.** Nothing runs while declaring; declarations must be deterministic — derive nothing from the clock or environment.
- **Forward-first.** Rollbacks come free for structural operations; irreversibility is an explicit, typed error.
- **No CLI.** Status, plans, and baselines are method calls, not a separate tool or login wall.
- **Convention with overrides.** Inferred parent tables, conventional constraint names, and portable column types each take an explicit override (`Constrained("people")`, `.Name("...")`, `t.Column(name, "any type")`, `s.Exec("any SQL")`).

## License

go-rio/migrate is released under the [MIT License](LICENSE), © 2026-now
TreeNewBee.
