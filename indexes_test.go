package migrate

import (
	"strings"
	"testing"
)

// compileErr compiles the declaration and returns the first error from
// validation or dialect compilation, or nil when everything compiles.
func compileErr(d Dialect, fn func(*Schema)) error {
	s := &Schema{}
	fn(s)
	if err := validateSchema("test", s); err != nil {
		return err
	}
	for _, op := range s.ops {
		if _, err := d.compile(op); err != nil {
			return err
		}
	}
	return nil
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q should mention %q", err, want)
	}
}

// The partial-index shape this feature exists for: uniqueness among live rows
// under soft deletion.
func TestPartialIndex(t *testing.T) {
	partial := func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.Unique("email").Where("deleted_at IS NULL")
		})
	}

	assertSQL(t, compileSchema(t, Postgres, partial), []string{
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email") WHERE deleted_at IS NULL`,
	})
	assertSQL(t, compileSchema(t, SQLite, partial), []string{
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email") WHERE deleted_at IS NULL`,
	})
	assertErrContains(t, compileErr(MySQL, partial), "partial indexes")
}

func TestPartialIndexInsideCreate(t *testing.T) {
	got := compileSchema(t, SQLite, func(s *Schema) {
		s.Create("users", func(t *Table) {
			t.ID()
			t.String("name")
			t.SoftDeletes()
			t.Unique("name").Where("deleted_at IS NULL")
		})
	})
	if want := `CREATE UNIQUE INDEX "users_name_unique" ON "users" ("name") WHERE deleted_at IS NULL`; got[len(got)-1] != want {
		t.Fatalf("last statement = %s, want %s", got[len(got)-1], want)
	}
}

func TestIndexUsingMethod(t *testing.T) {
	gin := func(s *Schema) {
		s.Table("events", func(t *Table) { t.Index("payload").Using("gin") })
	}
	assertSQL(t, compileSchema(t, Postgres, gin), []string{
		`CREATE INDEX "events_payload_index" ON "events" USING gin ("payload")`,
	})

	hash := func(s *Schema) {
		s.Table("events", func(t *Table) { t.Index("token").Using("hash") })
	}
	assertSQL(t, compileSchema(t, MySQL, hash), []string{
		"CREATE INDEX `events_token_index` ON `events` (`token`) USING HASH",
	})

	assertErrContains(t, compileErr(SQLite, gin), "single index type")
}

func TestExpressionIndex(t *testing.T) {
	lower := func(s *Schema) {
		s.Table("users", func(t *Table) {
			t.UniqueExpr("users_email_lower_unique", "lower(email)")
		})
	}

	assertSQL(t, compileSchema(t, Postgres, lower), []string{
		`CREATE UNIQUE INDEX "users_email_lower_unique" ON "users" ((lower(email)))`,
	})
	// MySQL requires each functional key part parenthesized; the shared
	// rendering already satisfies it.
	assertSQL(t, compileSchema(t, MySQL, lower), []string{
		"CREATE UNIQUE INDEX `users_email_lower_unique` ON `users` ((lower(email)))",
	})
	assertSQL(t, compileSchema(t, SQLite, lower), []string{
		`CREATE UNIQUE INDEX "users_email_lower_unique" ON "users" ((lower(email)))`,
	})
}

func TestExpressionIndexNeedsName(t *testing.T) {
	err := compileErr(SQLite, func(s *Schema) {
		s.Table("users", func(t *Table) { t.IndexExpr("", "lower(email)") })
	})
	assertErrContains(t, err, "needs an explicit name")
}

func TestFullTextIndex(t *testing.T) {
	fulltext := func(s *Schema) {
		s.Table("posts", func(t *Table) { t.FullText("title", "body") })
	}

	assertSQL(t, compileSchema(t, MySQL, fulltext), []string{
		"CREATE FULLTEXT INDEX `posts_title_body_fulltext` ON `posts` (`title`, `body`)",
	})
	assertErrContains(t, compileErr(Postgres, fulltext), "tsvector")
	assertErrContains(t, compileErr(SQLite, fulltext), "FTS5")

	drop := compileSchema(t, MySQL, func(s *Schema) {
		s.Table("posts", func(t *Table) { t.DropFullText("title", "body") })
	})
	assertSQL(t, drop, []string{"ALTER TABLE `posts` DROP INDEX `posts_title_body_fulltext`"})
}

func TestSpatialIndex(t *testing.T) {
	spatial := func(s *Schema) {
		s.Table("places", func(t *Table) { t.Spatial("location") })
	}

	assertSQL(t, compileSchema(t, MySQL, spatial), []string{
		"CREATE SPATIAL INDEX `places_location_spatial` ON `places` (`location`)",
	})
	assertErrContains(t, compileErr(Postgres, spatial), "PostGIS")

	drop := compileSchema(t, MySQL, func(s *Schema) {
		s.Table("places", func(t *Table) { t.DropSpatial("location") })
	})
	assertSQL(t, drop, []string{"ALTER TABLE `places` DROP INDEX `places_location_spatial`"})
}

func TestCoveringIndex(t *testing.T) {
	covering := func(s *Schema) {
		s.Table("users", func(t *Table) { t.Unique("email").Include("name", "created_at") })
	}

	assertSQL(t, compileSchema(t, Postgres, covering), []string{
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email") INCLUDE ("name", "created_at")`,
	})
	assertErrContains(t, compileErr(MySQL, covering), "INCLUDE")
	assertErrContains(t, compileErr(SQLite, covering), "INCLUDE")
}

func TestNullsNotDistinct(t *testing.T) {
	unique := func(s *Schema) {
		s.Table("users", func(t *Table) { t.Unique("email").NullsNotDistinct() })
	}
	assertSQL(t, compileSchema(t, Postgres, unique), []string{
		`CREATE UNIQUE INDEX "users_email_unique" ON "users" ("email") NULLS NOT DISTINCT`,
	})

	plain := func(s *Schema) {
		s.Table("users", func(t *Table) { t.Index("email").NullsNotDistinct() })
	}
	assertErrContains(t, compileErr(Postgres, plain), "unique indexes only")
	assertErrContains(t, compileErr(MySQL, unique), "NULLs")
}

func TestIndexOptionsCombine(t *testing.T) {
	got := compileSchema(t, Postgres, func(s *Schema) {
		s.Table("events", func(t *Table) {
			t.Index("user_id").Using("btree").Include("kind").Where("deleted_at IS NULL")
		})
	})
	assertSQL(t, got, []string{
		`CREATE INDEX "events_user_id_index" ON "events" USING btree ("user_id") INCLUDE ("kind") WHERE deleted_at IS NULL`,
	})
}

// Concurrently must pair with WithoutTransaction on Postgres; the compile gate
// reports the conflict instead of the server.
func TestConcurrentIndexNeedsWithoutTransaction(t *testing.T) {
	up := func(s *Schema) {
		s.Table("events", func(t *Table) { t.Index("user_id").Concurrently() })
	}

	tx := &Migration{name: "m", up: up, useTx: true}
	if _, err := tx.compile(Postgres, true); err == nil || !strings.Contains(err.Error(), "WithoutTransaction") {
		t.Fatalf("transactional compile error = %v, want mention of WithoutTransaction", err)
	}

	noTx := &Migration{name: "m", up: up, useTx: false}
	stmts, err := noTx.compile(Postgres, true)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	want := `CREATE INDEX CONCURRENTLY "events_user_id_index" ON "events" ("user_id")`
	if len(stmts) != 1 || stmts[0].sql != want {
		t.Fatalf("got %q, want %q", stmts[0].sql, want)
	}

	// The rollback drops concurrently too, inside the same untransacted run.
	downs, err := noTx.compile(Postgres, false)
	if err != nil {
		t.Fatalf("compile down: %v", err)
	}
	wantDown := `DROP INDEX CONCURRENTLY "events_user_id_index"`
	if len(downs) != 1 || downs[0].sql != wantDown {
		t.Fatalf("got %q, want %q", downs[0].sql, wantDown)
	}

	// MySQL and SQLite build indexes online anyway: the flag renders nothing
	// and transactional migrations stay legal.
	txOther := &Migration{name: "m", up: up, useTx: true}
	stmts, err = txOther.compile(SQLite, true)
	if err != nil {
		t.Fatalf("sqlite compile: %v", err)
	}
	if strings.Contains(stmts[0].sql, "CONCURRENTLY") {
		t.Fatalf("sqlite should ignore Concurrently, got %q", stmts[0].sql)
	}
}

// The checksum is the compiled SQL, so every new index attribute must move it.
func TestIndexOptionsMoveChecksum(t *testing.T) {
	base := &Migration{name: "m", useTx: true, up: func(s *Schema) {
		s.Table("users", func(t *Table) { t.Unique("email") })
	}}
	partial := &Migration{name: "m", useTx: true, up: func(s *Schema) {
		s.Table("users", func(t *Table) { t.Unique("email").Where("deleted_at IS NULL") })
	}}

	sumBase, err := base.checksum(SQLite)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	sumPartial, err := partial.checksum(SQLite)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if sumBase == sumPartial {
		t.Fatal("adding Where must change the checksum")
	}
}
