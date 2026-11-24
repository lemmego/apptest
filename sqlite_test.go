package apptest

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"

	// Imported HERE, in a test file, and deliberately not in sqlite.go.
	//
	// A test binary needs a registered "sqlite" driver to exercise the
	// harness, and a test file is the safe place to say so: it never reaches
	// a consumer. Putting it in sqlite.go would panic the test binary of any
	// application that chose modernc.org/sqlite instead, because both
	// packages register that same name. sqlite.go's doc comment records the
	// reasoning.
	_ "github.com/glebarez/go-sqlite"
)

// sqliteConfig is the configuration an application configured for SQLite
// would have. The placeholder path must never be written to, which is half of
// what these tests check.
func sqliteConfig() config.M {
	return config.M{
		"app": config.M{"name": "apptest", "env": "test", "port": 0},
		"sql": config.M{
			"default": "sqlite",
			"connections": config.M{
				"sqlite": config.M{
					"driver":   "sqlite",
					"database": "./storage/must-not-be-touched.sqlite",
					"prefix":   "",
				},
			},
		},
	}
}

// onlyThisMigration replaces the global migrator's list for one test and puts
// it back afterwards.
//
// Necessary because the migrator is process-wide and postgres_test.go
// registers a migration containing BIGSERIAL, which SQLite rejects. Without
// this, m.Up(-1) here would run that Postgres DDL against a SQLite file and
// fail for a reason that has nothing to do with the code under test — and
// which test ran first would decide it.
//
// It also restores, where postgres_test.go's `t.Cleanup(func() { _ =
// oldMigrator })` discards the value and restores nothing. The fields are
// assigned directly rather than through AddMigration, which takes the
// migrator's mutex; that is safe only because these tests are not parallel.
func onlyThisMigration(t *testing.T, m *migration.Migration) {
	t.Helper()
	mig := migration.GetMigrator()
	oldVersions, oldMigrations := mig.Versions, mig.Migrations
	t.Cleanup(func() { mig.Versions, mig.Migrations = oldVersions, oldMigrations })

	mig.Versions = nil
	mig.Migrations = map[string]*migration.Migration{}
	mig.AddMigration(m)
}

func probeMigration() *migration.Migration {
	return &migration.Migration{
		Version: "20260301000000",
		Up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE apptest_probe (id INTEGER PRIMARY KEY AUTOINCREMENT, note TEXT)`)
			return err
		},
		Down: func(tx *sql.Tx) error {
			_, err := tx.Exec(`DROP TABLE apptest_probe`)
			return err
		},
	}
}

func selectedSQLiteDatabase(t *testing.T, cfg config.M) string {
	t.Helper()
	conn := cfg["sql"].(config.M)["connections"].(config.M)["sqlite"].(config.M)
	path, _ := conn["database"].(string)
	return path
}

// The application must end up pointed at the scratch file, not at the path
// its configuration named. Getting this wrong means a suite writing into a
// developer's working database — the same hazard WithScratchDB guards on
// Postgres, and a worse one here, because a stray SQLite file is created
// silently rather than refused by a server.
func TestWithScratchSQLiteCreatesAndMigratesAScratchDatabase(t *testing.T) {
	onlyThisMigration(t, probeMigration())

	cfg := sqliteConfig()
	New(t, WithConfig(cfg), WithScratchSQLite())

	path := selectedSQLiteDatabase(t, cfg)
	if strings.Contains(path, "must-not-be-touched") {
		t.Fatalf("the configuration still names the placeholder: %q", path)
	}
	if filepath.Base(path) != "apptest.db" {
		t.Fatalf("sql.connections.sqlite.database = %q, want a scratch file", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the scratch database does not exist: %v", err)
	}

	// The global the connectors actually read, not just the map.
	if got := configString("sql.connections.sqlite.database"); got != path {
		t.Errorf("config singleton says %q, the map says %q", got, path)
	}

	// And the migration ran, in that file.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'apptest_probe'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("migrations did not run against the scratch database")
	}
}

// Two applications in one package must not share a file, or tests see each
// other's rows and start depending on order. One configuration map is passed
// to both, which is what a real harness does with config.GetAll().
func TestWithScratchSQLiteIsolatesEachApp(t *testing.T) {
	cfg := sqliteConfig()

	New(t, WithConfig(cfg), WithScratchSQLiteNoMigrate())
	first := selectedSQLiteDatabase(t, cfg)

	New(t, WithConfig(cfg), WithScratchSQLiteNoMigrate())
	second := selectedSQLiteDatabase(t, cfg)

	if first == "" || first == second {
		t.Fatalf("both apps got %q", first)
	}
}

// The file goes when the test ends, and so do its -wal and -shm siblings.
//
// That is the whole reason the scratch database lives in t.TempDir rather
// than being a file this package removes itself: an os.Remove of the database
// alone leaves the write-ahead log and shared-memory files behind, and the
// next run opening a fresh database beside a stale -wal is the kind of
// corruption that reads as "SQLite is flaky".
func TestTheScratchFileAndItsSiblingsAreGoneAfterTheTest(t *testing.T) {
	var path string

	t.Run("inner", func(t *testing.T) {
		cfg := sqliteConfig()
		New(t, WithConfig(cfg), WithScratchSQLiteNoMigrate())
		path = selectedSQLiteDatabase(t, cfg)

		// Force SQLite to create the -wal sibling, so the assertion below is
		// about something that existed.
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE t (id INTEGER); INSERT INTO t VALUES (1)`); err != nil {
			t.Fatal(err)
		}
	})

	if path == "" {
		t.Fatal("the inner test did not report a path")
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Dir(path)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after the test (err=%v)", p, err)
		}
	}
}

// pointConfigAtSQLite is the part most likely to be handed a configuration it
// cannot use, and each refusal has to name the problem rather than panic on a
// type assertion. The driver case matters most: it is what stops somebody
// calling the SQLite option on a Postgres application and getting a database
// file instead of an error.
func TestPointConfigAtSQLiteRejectsUnusableConfigurations(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.M
		want string
	}{
		{"no sql block", config.M{}, "needs the application's sql config"},
		{"no connections", config.M{"sql": config.M{}}, "sql.connections is missing"},
		{
			"no default",
			config.M{"sql": config.M{"connections": config.M{}}},
			"sql.default is not set",
		},
		{
			"default names a missing connection",
			config.M{"sql": config.M{"default": "nope", "connections": config.M{}}},
			"sql.connections.nope is missing",
		},
		{
			"a postgres connection",
			config.M{"sql": config.M{"default": "pgsql", "connections": config.M{
				"pgsql": config.M{"driver": "postgres"},
			}}},
			`has driver "postgres"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := &fatalRecorder{}
			pointConfigAtSQLite(recorder, c.cfg, "/tmp/ignored.db")
			if !strings.Contains(recorder.message, c.want) {
				t.Errorf("message = %q, want it to contain %q", recorder.message, c.want)
			}
		})
	}
}

// The restore has to put both places back, or the next test in the package
// boots against a scratch file that has already been deleted — a test that
// passes alone and fails in the package, which is the hardest kind to
// attribute. pointConfigAt documents the same hazard for Postgres.
func TestPointConfigAtSQLiteRestoresBothPlaces(t *testing.T) {
	cfg := sqliteConfig()
	original := selectedSQLiteDatabase(t, cfg)

	restore := pointConfigAtSQLite(t, cfg, "/tmp/scratch.db")
	if got := selectedSQLiteDatabase(t, cfg); got != "/tmp/scratch.db" {
		t.Fatalf("map was not pointed at the scratch file, got %q", got)
	}
	if got := configString("sql.connections.sqlite.database"); got != "/tmp/scratch.db" {
		t.Fatalf("singleton was not pointed at the scratch file, got %q", got)
	}

	restore()
	if got := selectedSQLiteDatabase(t, cfg); got != original {
		t.Errorf("map = %q after restore, want %q", got, original)
	}
	if got := configString("sql.connections.sqlite.database"); got != original {
		t.Errorf("singleton = %q after restore, want %q", got, original)
	}
}

// configString reads the package-level singleton the connectors use.
// config.Get returns any, so the assertion lives here rather than at three
// call sites.
func configString(key string) string {
	value, _ := config.Get(key).(string)
	return value
}
