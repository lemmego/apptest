package apptest

import (
	"database/sql"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"

	// In a test file, never in libsql.go, for the reason sqlite.go records at
	// length: a harness that imports a driver panics the test binary of any
	// application that registered the same name from another package.
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

const testLibSQLURL = "http://127.0.0.1:8080"

func libsqlConfig() config.M {
	return config.M{
		"app": config.M{"name": "apptest", "env": "test", "port": 0},
		"sql": config.M{
			"default": "libsql",
			"connections": config.M{
				"libsql": config.M{
					"driver": "libsql",
					"url":    testLibSQLURL,
				},
			},
		},
	}
}

// Skipped rather than failed, on the same reasoning postgres_test.go gives for
// skipWithoutPostgres: this runs in apptest's own suite, where the CI that
// matters supplies the service and a contributor without one is not owed a
// failure. The skip that WithScratchDB used to do in OTHER people's suites is
// a different decision and still wrong.
func skipWithoutLibSQL(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:8080", 500*time.Millisecond)
	if err != nil {
		t.Skipf("apptest: no libSQL server on 127.0.0.1:8080 (%v); run one with "+
			"docker run -p 8080:8080 -e SQLD_NODE=primary ghcr.io/tursodatabase/libsql-server", err)
	}
	_ = conn.Close()
}

func libsqlProbeMigration() *migration.Migration {
	return &migration.Migration{
		Version: "20260401000000",
		Up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS apptest_libsql_probe (
				id INTEGER PRIMARY KEY AUTOINCREMENT, note TEXT)`)
			return err
		},
		Down: func(tx *sql.Tx) error {
			_, err := tx.Exec(`DROP TABLE apptest_libsql_probe`)
			return err
		},
	}
}

func openTestLibSQL(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(DriverLibSQL, testLibSQLURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Migrations run against the server, and the tables start empty.
func TestWithScratchLibSQLMigratesAndEmpties(t *testing.T) {
	skipWithoutLibSQL(t)
	onlyThisMigration(t, libsqlProbeMigration())

	db := openTestLibSQL(t)

	// A row left behind by a previous test, which the harness must remove.
	New(t, WithConfig(libsqlConfig()), WithScratchLibSQL())
	if _, err := db.Exec(`INSERT INTO apptest_libsql_probe (note) VALUES ('left behind')`); err != nil {
		t.Fatal(err)
	}

	// A second application in the same package: the schema is still there and
	// the row is gone.
	New(t, WithConfig(libsqlConfig()), WithScratchLibSQL())

	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'apptest_libsql_probe'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 1 {
		t.Error("the migration did not run, or the table was dropped rather than emptied")
	}

	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM apptest_libsql_probe`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("%d row(s) survived into the next test", rows)
	}
}

// schema_migrations must survive, or every test re-runs every migration and
// the cheapest part of the harness becomes the most expensive.
func TestEmptyingLeavesSchemaMigrationsAlone(t *testing.T) {
	skipWithoutLibSQL(t)
	onlyThisMigration(t, libsqlProbeMigration())

	New(t, WithConfig(libsqlConfig()), WithScratchLibSQL())

	db := openTestLibSQL(t)
	var applied int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied == 0 {
		t.Error("schema_migrations was emptied, so the next test will re-run every migration")
	}
}

// A configuration without a url cannot name a libSQL server, and the refusal
// has to say so rather than opening something unexpected.
func TestLibSQLRefusesUnusableConfigurations(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  config.M
		want string
	}{
		{"no sql block", config.M{}, "needs the application's sql config"},
		{
			"a sqlite connection",
			config.M{"sql": config.M{"default": "sqlite", "connections": config.M{
				"sqlite": config.M{"driver": "sqlite", "database": "x.db"},
			}}},
			`has driver "sqlite"`,
		},
		{
			"libsql with no url",
			config.M{"sql": config.M{"default": "libsql", "connections": config.M{
				"libsql": config.M{"driver": "libsql"},
			}}},
			"needs a url",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorder := &fatalRecorder{}
			libsqlURLFromConfig(recorder, c.cfg)
			if !containsString(recorder.message, c.want) {
				t.Errorf("message = %q, want it to contain %q", recorder.message, c.want)
			}
		})
	}
}

// The three scratch options describe one connection each, so asking for more
// than one is a mistake in the test rather than a preference to resolve.
func TestTheScratchOptionsAreMutuallyExclusive(t *testing.T) {
	if countTrue(false, false, false) != 0 {
		t.Error("countTrue(none) should be 0")
	}
	if countTrue(true, false, true) != 2 {
		t.Error("countTrue should count every set flag")
	}
}

func containsString(haystack, needle string) bool { return strings.Contains(haystack, needle) }
