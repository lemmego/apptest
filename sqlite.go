package apptest

import (
	"database/sql"
	"path/filepath"
	"slices"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

// WithScratchSQLite gives the test its own SQLite database file, migrated and
// then removed.
//
// It creates a database in a per-test temporary directory, points the
// application's configuration at it, runs every migration registered with
// migration.GetMigrator, and lets the directory go when the test ends. Tests
// therefore do not see each other's rows and can run in any order — the same
// contract WithScratchDB offers on Postgres.
//
// # Why this exists
//
// The harness was Postgres-only, which meant a project configured for SQLite
// had no way to boot a real application against a real schema in a test. That
// is also what blocked evaluating any member of the distributed-SQLite family
// — LiteFS, rqlite, libSQL — because each of them presents SQLite to the
// application and none of them could be tested.
//
// # Why it needs no teardown of its own
//
// t.TempDir removes the directory when the test ends, and that takes the
// database with it along with its -wal and -shm siblings, which a bare
// os.Remove of the database file would leave behind. So unlike the Postgres
// path there is no drop to register, no interrupt handler to install, and
// nothing to leak onto a shared server: the worst an interrupted run leaves
// is a directory under TMPDIR that the operating system reclaims.
//
// # Why no driver is blank-imported here
//
// postgres.go blank-imports lib/pq, on the reasoning that a caller should not
// have to import a driver to make a harness's internals work. That reasoning
// does not survive contact with SQLite, and the failure it would cause is
// worse than the inconvenience it would save: github.com/glebarez/go-sqlite
// and modernc.org/sqlite BOTH register the driver name "sqlite", and
// sql.Register panics when a name is registered twice. This monorepo uses
// both. So a blank import here would take down the whole test binary of any
// application that happened to choose the other one — a panic in init, before
// a single test runs, pointing at a harness the author never edited.
//
// Instead the driver is the application's to register, exactly as
// ormconnector and migration already require, and its absence is reported
// with a sentence that says what to do.
func WithScratchSQLite() Option {
	return func(o *options) {
		o.sqlite = true
		o.migrate = true
	}
}

// WithScratchSQLiteNoMigrate is WithScratchSQLite without running
// migrations, for a test that wants to exercise the migrator itself.
func WithScratchSQLiteNoMigrate() Option {
	return func(o *options) {
		o.sqlite = true
		o.migrate = false
	}
}

func (a *App) setUpSQLite(o *options) {
	a.t.Helper()

	// Refuse before touching anything, with the fix in the message. Opening
	// an unregistered driver fails later with database/sql's own "unknown
	// driver" text, which names neither this option nor the import that would
	// satisfy it.
	if !slices.Contains(sql.Drivers(), migration.DriverSQLite) {
		a.t.Fatalf("apptest: WithScratchSQLite needs a %q driver registered; "+
			"blank-import the one your application uses — "+
			`_ "github.com/glebarez/go-sqlite" or _ "modernc.org/sqlite" — `+
			"in the test package. This harness deliberately imports neither, "+
			"because both register that same name and sql.Register panics on "+
			"the second one.", migration.DriverSQLite)
		return
	}

	// A directory rather than a bare file, so the -wal and -shm siblings go
	// with it. The name is fixed because the directory is already unique per
	// test.
	path := filepath.Join(a.t.TempDir(), "apptest.db")

	restore := pointConfigAtSQLite(a.t, o.config, path)
	a.t.Cleanup(restore)

	if o.migrate {
		a.runSQLiteMigrations(path)
	}
}

func (a *App) runSQLiteMigrations(path string) {
	a.t.Helper()

	// The same trap postgres.go documents, in the other direction:
	// migration.Create resolves its dialect from DB_DRIVER when a migration's
	// Up runs, and defaults to sqlite when it is unset. That default happens
	// to be right here, but relying on it would mean a suite that passes for
	// the wrong reason and breaks the moment anything sets DB_DRIVER — which
	// the Postgres path in this very package does, with t.Setenv.
	a.t.Setenv("DB_DRIVER", migration.DriverSQLite)

	db, err := sql.Open(migration.DriverSQLite, path)
	if err != nil {
		a.t.Fatalf("apptest: opening the scratch database: %v", err)
	}
	defer db.Close()

	m, err := migration.Init(db, migration.DriverSQLite)
	if err != nil {
		a.t.Fatalf("apptest: initialising the migrator: %v", err)
	}
	// -1 runs everything pending, which is what a fresh database needs.
	if err := m.Up(-1); err != nil {
		a.t.Fatalf("apptest: running migrations: %v", err)
	}
}

// pointConfigAtSQLite rewrites the sql block so the application's connector
// opens the scratch database file.
//
// It writes to the configuration map the test passed AND to the package-level
// config singleton, for the two reasons pointConfigAt sets out: the
// application boots from the map, while ormconnector.sqlConfig and its
// siblings resolve the global through config.Get.
func pointConfigAtSQLite(t testingT, cfg config.M, path string) (restore func()) {
	t.Helper()

	block, ok := cfg["sql"].(config.M)
	if !ok {
		t.Fatalf("apptest: WithScratchSQLite needs the application's sql config; pass WithConfig(config.GetAll())")
		return func() {}
	}
	connections, ok := block["connections"].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections is missing from the configuration")
		return func() {}
	}
	selected, _ := block["default"].(string)
	if selected == "" {
		t.Fatalf("apptest: sql.default is not set, so there is no connection to point at the scratch database")
		return func() {}
	}
	conn, ok := connections[selected].(config.M)
	if !ok {
		t.Fatalf("apptest: sql.connections.%s is missing; WithScratchSQLite needs a connection block", selected)
		return func() {}
	}
	if driver, _ := conn["driver"].(string); driver != migration.DriverSQLite {
		t.Fatalf("apptest: sql.connections.%s has driver %q; WithScratchSQLite is for %q — use WithScratchDB for postgres",
			selected, driver, migration.DriverSQLite)
		return func() {}
	}

	// Only the path changes. Every other key in the block is the
	// application's own, which is the same argument pointConfigAt makes: two
	// sources for one setting is one thing that can disagree.
	previous := conn["database"]
	previousDefault, _ := block["default"]

	key := "sql.connections." + selected + ".database"
	conn["database"] = path
	config.Set(key, path)
	config.Set("sql.default", selected)

	return func() {
		conn["database"] = previous
		config.Set(key, previous)
		config.Set("sql.default", previousDefault)
	}
}
