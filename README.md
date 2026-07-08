# migrate

[![Doc](https://pkg.go.dev/badge/github.com/libtnb/migrate)](https://pkg.go.dev/github.com/libtnb/migrate)
[![Go](https://img.shields.io/github/go-mod/go-version/libtnb/migrate)](https://go.dev/)
[![Release](https://img.shields.io/github/release/libtnb/migrate.svg)](https://github.com/libtnb/migrate/releases)
[![Test](https://github.com/libtnb/migrate/actions/workflows/test.yml/badge.svg)](https://github.com/libtnb/migrate/actions)
[![Report Card](https://goreportcard.com/badge/github.com/libtnb/migrate)](https://goreportcard.com/report/github.com/libtnb/migrate)
[![Stars](https://img.shields.io/github/stars/libtnb/migrate?style=flat)](https://github.com/libtnb/migrate)
[![License](https://img.shields.io/github/license/libtnb/migrate)](https://opensource.org/license/MIT)

Database schema migrations written as Go code and compiled into your binary —
no SQL files to ship, no CLI to install, no third-party dependencies.

A migration declares its changes once, on a fluent schema builder. Because the
declaration is data rather than opaque SQL, one declaration gives you all of
this: dialect-specific SQL for PostgreSQL, MySQL and SQLite; an automatic
rollback with no hand-written down migration; a reviewable dry-run plan; and a
checksum that catches migrations edited after they ran.

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

- **Migrations are code.** Each migration is a Go file that registers itself
  in `init`; `go build` packages the entire migration history into the binary.
  Deploying the binary deploys the migrations — nothing to copy, embed or
  mount, and a broken migration is a compile error, not a runtime surprise.
- **Automatic rollbacks.** `Rollback` reverses the recorded operations in
  reverse order: a created table drops, a rename renames back, an added index
  disappears before the column it covers. Operations that discard information
  (dropping tables or columns, raw SQL, Go functions) refuse to guess: rolling
  them back requires an explicit `WithDown`, and fails with `ErrIrreversible`
  otherwise.
- **No dirty state, ever.** Each migration row is written atomically with the
  migration itself. On PostgreSQL and SQLite a failure rolls the whole
  migration back — schema and bookkeeping together — and the next `Up` simply
  retries. There is no flag to `force` clear by hand. MySQL commits DDL
  implicitly and cannot be made atomic; failures there say exactly which
  statement failed and that earlier DDL is already in effect.
- **Safe under concurrency by default.** Replicas racing to migrate at deploy
  time are serialized with a session-level advisory lock (`pg_advisory_lock`,
  `GET_LOCK`) held on a dedicated connection — released by the database itself
  if a migrator crashes, so a killed pod never wedges the next deploy. Opt out
  with `WithoutLock` when a deploy job already guarantees a single runner.
- **Tamper detection.** Every applied migration records a checksum of the SQL
  it compiled to. If a migration changes after it ran, `Up` warns (or fails,
  under `WithStrictChecksum`), `Status` reports the drift, and `Repair`
  re-records current checksums after a reviewed change.
- **Repeatable migrations.** Views, stored functions, triggers and reference
  data registered with `AddRepeatable` re-run whenever their declaration
  changes — edit the definition in place instead of writing
  `create_view_v2, _v3, …` migrations. They run after all versioned
  migrations; rollbacks never touch them.
- **Reviewable before it runs.** `Plan` renders the SQL of pending migrations
  against a live database; `Collection.SQL` renders a whole collection offline
  for DBA review with no database at all; `PlanRollback` previews a rollback.
- **Dialects that fail loud, not silent.** The builder compiles to each
  engine's best-practice types (`JSONB`, `TIMESTAMPTZ`, identity columns on
  Postgres; `DATETIME(6)`, native `ENUM` on MySQL). Where an engine cannot do
  something — SQLite altering constraints — compiling returns a clear error
  instead of silently skipping the change.
- **Batch-aware history.** Each `Up` run is one batch; `Rollback` undoes the
  latest batch, `Steps(n)` undoes exactly n migrations, `Reset` undoes
  everything. `Baseline` adopts a pre-existing database without executing
  anything.
- **Zero dependencies.** Only `database/sql` and the standard library: you
  pass in the `*sql.DB` you already have, with whatever driver you chose.

## Installation

```bash
go get github.com/libtnb/migrate
```

## Writing migrations

The conventional layout is a `migrations` package, one file per migration,
imported for effect from `main`:

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

Names order migrations lexically and are recorded in the database, so start
them with a sortable timestamp. Registration panics on duplicate or malformed
names at init time — a broken migration set stops the program before it
touches anything.

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
`.Default(v)`, `.DefaultExpr(expr)`, `.UseCurrent()`, `.Unsigned()`,
`.Unique()`, `.Index()`, `.Primary()`, `.AutoIncrement()`, `.Comment(...)`,
`.StoredAs(expr)`/`.VirtualAs(expr)` for generated columns, and MySQL's
`.After(...)`/`.First()`/`.UseCurrentOnUpdate()`.

Table names may be schema-qualified — `s.Create("analytics.events", ...)`
renders `"analytics"."events"` and conventional constraint names stay inside
the schema. Named CHECK constraints round out the constraint set (anonymous
checks are rejected — an unnamed constraint cannot be dropped later):

```go
t.Check("orders_price_positive", "price > 0")   // in Create or Table
t.DropCheck("orders_price_positive")            // reverses an added check
```

`.AutoIncrement()` turns any integer column into the database-generated
primary key, preferring each engine's generated-column form: an SQL-standard
identity column on Postgres (`GENERATED BY DEFAULT AS IDENTITY`, not legacy
serial), `AUTO_INCREMENT` on MySQL, `INTEGER PRIMARY KEY AUTOINCREMENT` on
SQLite. `t.ID()` is the conventional shorthand for
`t.BigInteger("id").Unsigned().AutoIncrement()`.

### Indexes and foreign keys

```go
t.Index("a", "b")                 // articles_a_b_index
t.Unique("slug").Name("custom")   // custom name
t.Primary("a", "b")               // composite primary key

t.ForeignID("user_id").Constrained()              // → users.id, inferred
t.ForeignID("category_id").Constrained().NullOnDelete()
t.Foreign("code").References("regions", "code")   // existing column, explicit
```

Index and constraint names follow one convention —
`{table}_{columns}_{index|unique|foreign}` — so dropping by columns
(`t.DropIndex("a", "b")`, `t.DropForeign("user_id")`) reconstructs the same
name that creating produced.

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

Each change compiles to its own statement and reverses individually; the
whole migration still runs in one transaction where the engine allows it.
Also available: `t.RenameIndex(from, to)`, `t.DropIndex/DropUnique/DropForeign`
(by columns, via the conventional names), `t.DropPrimary()`, and table
comments with `t.Comment(...)`.

### Rebuilding tables

What `ALTER TABLE` cannot express — on SQLite, any constraint change —
`Recreate` can: declare the complete target table and the migrator rebuilds
it around the data (create temporary → copy rows → drop old → rename →
rebuild indexes), inside the migration's transaction on Postgres and SQLite:

```go
s.Recreate("users", func(t *migrate.Table) {
	t.ID()
	t.String("email").Unique()                                 // the new constraint
	t.Integer("logins").Default(0).SkipCopy()                  // brand-new column
})
```

Columns copy by name; `SkipCopy` marks ones the old table does not have, and
`CopyFrom` substitutes a SELECT expression — renaming a column and converting
its type in one rebuild:

```go
t.Integer("age").CopyFrom("CAST(age AS INTEGER)")
```
Conventional constraint and index names come out for the *final* table name,
so later `DropUnique`/`DropForeign` calls still resolve. Recreate discards
the old definition, so rolling back needs a `WithDown` (usually another
`Recreate` declaring the previous shape). One SQLite caveat: with
`PRAGMA foreign_keys=ON` and child rows referencing the table, run the
migration on a connection with enforcement off — enforcement is off by
default in SQLite and most drivers.

There is deliberately no `Change()` for redefining a column's type in place.
That API is where migration tools quietly lose modifiers, and SQLite cannot
do it at all without rebuilding the table. Say what you mean with SQL — it
stays reviewable in `Plan` and checksummed like everything else:

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

`Run` receives the migration's transaction when there is one; `migrate.DB` is
satisfied by both `*sql.Tx` and `*sql.DB`, so the code reads the same either
way. Statements that refuse to run inside a transaction — most famously
`CREATE INDEX CONCURRENTLY` — mark the migration `migrate.WithoutTransaction()`:

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

A versioned migration runs once; a repeatable migration runs whenever its
compiled SQL differs from what was last recorded. That is the natural home
for anything defined by replacement — views, stored functions, triggers,
reference data — where history is noise and only the current definition
matters:

```go
migrate.AddRepeatable("active_users_view", func(s *migrate.Schema) {
	s.Exec(`CREATE OR REPLACE VIEW active_users AS
	        SELECT * FROM users WHERE deleted_at IS NULL`)
})
```

Editing the view is the whole workflow: change the declaration, deploy, and
the next `Up` re-runs it (after all versioned migrations, in name order).
Declarations must be idempotent — `CREATE OR REPLACE`, or `DROP ... IF EXISTS`
followed by `CREATE` on SQLite, which has no `OR REPLACE`. `Status` shows a
changed repeatable as drifted-pending, `Plan` renders what would re-run, and
rollbacks leave repeatables untouched (`Reset` forgets their records so a
fresh `Up` runs them again). A `Run` function's body is invisible to the
checksum, so edit SQL, not Go, when the change should trigger a re-run.

Two edges worth knowing: rolling back a *versioned* migration whose table a
repeatable view depends on is refused by Postgres while the view exists —
drop the dependent object first (deliberate: cascading silently would destroy
objects you did not name). And on databases without `CREATE OR REPLACE`
(SQLite), declare idempotency as `DROP ... IF EXISTS` + `CREATE`.

## Running

| Call | Effect |
|---|---|
| `m.Up(ctx)` | apply all pending migrations as one batch |
| `m.Rollback(ctx)` | undo the latest batch |
| `m.Rollback(ctx, migrate.Steps(2))` | undo the two most recent migrations |
| `m.Reset(ctx)` | undo everything |
| `m.Status(ctx)` | applied / pending / drifted / unregistered, per migration |
| `m.Plan(ctx)` / `m.PlanRollback(ctx)` | the SQL that would run, without running it |
| `m.Baseline(ctx)` | mark migrations applied without executing (existing databases) |
| `m.Repair(ctx)` | re-record checksums after a reviewed change |
| `m.Fresh(ctx)` | **development only**: drop every table, re-run everything |

Typical wiring runs `Up` at startup — safe with multiple replicas thanks to
the advisory lock — or from a dedicated deploy step:

```go
m, err := migrate.New(db, migrate.Postgres,
	migrate.WithLogger(slog.Default()),
	migrate.WithLockTimeout(2*time.Minute), // wait for a sibling deploy
)
```

Options: `WithCollection` (explicit collection instead of the global
registry), `WithTable` (records table name), `WithoutLock`, `WithLockTimeout`,
`WithStrictChecksum`, `WithLogger`, `WithClock`.

## Safety analysis

Some operations are harmless on an empty development database and incidents
on a loaded production one: dropping a column running code still reads,
adding a `NOT NULL` column to a populated table, building an index that
blocks writes. Because declarations are data, the migrator sees these before
executing anything:

- **`SafetyWarn`** (default) logs each finding through `WithLogger` and
  proceeds.
- **`WithSafety(migrate.SafetyStrict)`** makes `Up` fail with `ErrUnsafe`
  before executing anything, listing every finding across the run — wire it
  into CI.
- **`Assured()`** marks a reviewed migration so the analysis skips it; the
  finding was considered and accepted (small table, maintenance window, code
  already deployed).

`Plan` attaches the findings to each planned migration. Creating tables never
warns, and raw `Exec`/`Run` are deliberately not second-guessed — the
analysis covers the declarative operations where it can be precise:
destructive drops, backward-incompatible renames, `NOT NULL` additions
without defaults, and Postgres index/foreign-key builds that lock large
tables (with the `CONCURRENTLY` / `NOT VALID` escape routes in the message).

## Transactions and engines

| | PostgreSQL | MySQL | SQLite |
|---|---|---|---|
| DDL in transactions | yes — failed migrations roll back completely | no — DDL commits implicitly | yes — failed migrations roll back completely |
| Advisory lock | `pg_advisory_lock`, session-level | `GET_LOCK`, session-level | not needed (single-writer file) |
| Altering constraints | full support | full support | compile-time error with guidance |

Each migration runs in its own transaction by default. On MySQL the
transaction still protects data statements, but a failure mid-migration
leaves earlier DDL in effect — the error says so explicitly, names the failing
statement, and the migration is not recorded, so a fixed version simply runs
again. Keep MySQL migrations to a single DDL statement when you can.

One caveat worth knowing: session-level advisory locks do not survive
transaction-pooling proxies (PgBouncer in transaction mode). Point the
migrator at the database directly or through a session-mode pool.

## Design notes

- **Declarations are pure data.** A migration function only records
  operations; nothing executes while declaring. This one property is what
  makes automatic reversal, offline rendering, checksums and dialect
  portability all fall out of the same code path. It also means declarations
  must be deterministic — derive nothing from the clock or environment.
- **Forward-first, reversible when it's free.** Rollbacks exist for
  development flow and staged deploys, and they come for free for structural
  operations. But nothing pretends a destructive operation can be undone:
  irreversibility is an explicit, typed error, not a runtime surprise.
- **The library is the whole product.** There is no companion CLI to install
  and no login wall: anything a CLI would do — status, plans, baselines — is a
  method call you can wire into your own tooling in a few lines.
- **Convention with an exit hatch everywhere.** Inferred parent tables,
  conventional constraint names and portable column types cover the common
  case; every one of them accepts an explicit override (`Constrained("people")`,
  `.Name("...")`, `t.Column(name, "any type")`, `s.Exec("any SQL")`).

## License

libtnb/migrate is released under the [MIT License](LICENSE), © 2026-now
TreeNewBee.
