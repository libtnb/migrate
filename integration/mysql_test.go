package integration

import (
	"database/sql"
	"os"
	"testing"

	"github.com/go-rio/migrate"

	_ "github.com/go-sql-driver/mysql"
)

// openMySQL connects to MIGRATE_MYSQL_DSN or skips, e.g.
// root:root@tcp(localhost:3306)/migrate_test
func openMySQL(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_MYSQL_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_MYSQL_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("ping mysql: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMySQLEndToEnd(t *testing.T)      { runEndToEnd(t, openMySQL(t), migrate.MySQL) }
func TestMySQLChecksumFlow(t *testing.T)  { runChecksumFlow(t, openMySQL(t), migrate.MySQL) }
func TestMySQLDataMigration(t *testing.T) { runDataMigration(t, openMySQL(t), migrate.MySQL) }
func TestMySQLBaseline(t *testing.T)      { runBaseline(t, openMySQL(t), migrate.MySQL) }

func TestMySQLRepeatable(t *testing.T) { runRepeatable(t, openMySQL(t), migrate.MySQL) }
