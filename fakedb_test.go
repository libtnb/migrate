package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
)

// fakeDB is an in-memory database/sql driver that records every statement and
// serves programmed result rows, so runner behaviour — locking, transactions,
// bookkeeping, failure handling — asserts on exact statement sequences
// without a real database.
type fakeDB struct {
	mu       sync.Mutex
	log      []string
	args     [][]driver.NamedValue
	records  []record // rows served for records-table SELECTs
	failOn   string   // substring that makes Exec/Query fail
	failErr  error
	denyLock bool // advisory lock attempts report "not acquired"
}

func newFakeDB() *fakeDB { return &fakeDB{} }

// open returns a *sql.DB backed by this fake.
func (f *fakeDB) open() *sql.DB { return sql.OpenDB(fakeConnector{f}) }

func (f *fakeDB) logged() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeDB) record(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, entry)
}

// loggedContaining returns log entries containing the substring.
func (f *fakeDB) loggedContaining(sub string) []string {
	var out []string
	for _, l := range f.logged() {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeDB) fail(substr string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOn, f.failErr = substr, err
}

func (f *fakeDB) errFor(query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && strings.Contains(query, f.failOn) {
		return f.failErr
	}
	return nil
}

func (f *fakeDB) setRecords(recs ...record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = recs
}

type fakeConnector struct{ db *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{db: c.db}, nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("use sql.OpenDB")
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakeConn: Prepare not supported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.db.record("BEGIN")
	return fakeTx{c.db}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.db.errFor(query); err != nil {
		return nil, err
	}
	c.db.mu.Lock()
	c.db.log = append(c.db.log, query)
	c.db.args = append(c.db.args, args)
	c.db.mu.Unlock()
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := c.db.errFor(query); err != nil {
		return nil, err
	}
	c.db.record(query)
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	switch {
	case strings.Contains(query, "pg_try_advisory_lock"), strings.Contains(query, "GET_LOCK"):
		acquired := int64(1)
		if c.db.denyLock {
			acquired = 0
		}
		if strings.Contains(query, "pg_") {
			return &fakeRows{cols: []string{"ok"}, rows: [][]driver.Value{{!c.db.denyLock}}}, nil
		}
		return &fakeRows{cols: []string{"ok"}, rows: [][]driver.Value{{acquired}}}, nil
	case strings.Contains(query, "pg_advisory_unlock"):
		return &fakeRows{cols: []string{"ok"}, rows: [][]driver.Value{{true}}}, nil
	case strings.Contains(query, "RELEASE_LOCK"):
		return &fakeRows{cols: []string{"ok"}, rows: [][]driver.Value{{int64(1)}}}, nil
	case strings.Contains(query, "SELECT version, batch, checksum, applied_at"):
		rows := make([][]driver.Value, len(c.db.records))
		for i, r := range c.db.records {
			rows[i] = []driver.Value{r.version, int64(r.batch), r.checksum, r.appliedAt}
		}
		return &fakeRows{cols: []string{"version", "batch", "checksum", "applied_at"}, rows: rows}, nil
	default:
		return &fakeRows{cols: []string{}}, nil
	}
}

type fakeTx struct{ db *fakeDB }

func (t fakeTx) Commit() error   { t.db.record("COMMIT"); return nil }
func (t fakeTx) Rollback() error { t.db.record("ROLLBACK"); return nil }

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}
