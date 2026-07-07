package migrate

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	defaultTable       = "schema_migrations"
	defaultLockTimeout = time.Minute
)

// Clock supplies the current time for applied-at records. Tests can
// substitute a fake via WithClock.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Option configures a Migrator. Options are applied by New and validated
// together; an invalid combination makes New return an error instead of
// degrading silently at runtime.
type Option func(*config)

type config struct {
	collection     *Collection
	table          string
	lock           bool
	lockTimeout    time.Duration
	strictChecksum bool
	safety         SafetyLevel
	logger         *slog.Logger
	clock          Clock
}

func defaultConfig() config {
	return config{
		collection:  defaultCollection,
		table:       defaultTable,
		lock:        true,
		lockTimeout: defaultLockTimeout,
		logger:      slog.New(slog.DiscardHandler),
		clock:       systemClock{},
	}
}

// WithCollection uses an explicit migration collection instead of the default
// one that the package-level Add registers into.
func WithCollection(c *Collection) Option {
	return func(cfg *config) { cfg.collection = c }
}

// WithTable sets the name of the migration records table. The default is
// "schema_migrations". The advisory lock is derived from this name, so two
// migrators using different tables on one database do not exclude each other.
func WithTable(name string) Option {
	return func(cfg *config) { cfg.table = name }
}

// WithoutLock disables the advisory lock that serializes concurrent
// migrators. Only do this when something else already guarantees a single
// migrator, such as a deployment job that runs exactly once.
func WithoutLock() Option {
	return func(cfg *config) { cfg.lock = false }
}

// WithLockTimeout sets how long to wait for another migrator to release the
// advisory lock before failing with ErrLockTimeout. The default is one
// minute.
func WithLockTimeout(d time.Duration) Option {
	return func(cfg *config) { cfg.lockTimeout = d }
}

// WithStrictChecksum makes Up fail with ErrChecksumMismatch when an applied
// migration no longer compiles to the SQL it was applied with. The default
// only logs a warning, because the compiled SQL can also drift legitimately —
// for example when a new version of this package renders a clause
// differently; Repair re-records the current checksums after such an upgrade.
func WithStrictChecksum() Option {
	return func(cfg *config) { cfg.strictChecksum = true }
}

// WithLogger sets the logger for migration progress and warnings. The default
// discards everything.
func WithLogger(l *slog.Logger) Option {
	return func(cfg *config) { cfg.logger = l }
}

// WithClock substitutes the time source used for applied-at records, letting
// tests pin time.
func WithClock(c Clock) Option {
	return func(cfg *config) { cfg.clock = c }
}

func (c *config) validate() error {
	if c.collection == nil {
		return errors.New("migrate: collection must not be nil")
	}
	if c.table == "" || len(c.table) > 64 || strings.ContainsAny(c.table, "`\"'\x00") {
		return fmt.Errorf("migrate: invalid records table name %q", c.table)
	}
	if c.lockTimeout <= 0 {
		return errors.New("migrate: lock timeout must be positive")
	}
	if c.logger == nil {
		return errors.New("migrate: logger must not be nil")
	}
	if c.clock == nil {
		c.clock = systemClock{}
	}
	return nil
}
