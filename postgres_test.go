package apptest

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/lemmego/api/config"
	"github.com/lemmego/migration"
)

func postgresConfig() config.M {
	return config.M{
		"app": config.M{"name": "apptest", "env": "test", "port": 0},
		"sql": config.M{
			"default": "postgres",
			"connections": config.M{
				"postgres": config.M{
					"driver":   "postgres",
					"host":     "unset",
					"port":     5432,
					"database": "unset",
					"user":     "unset",
					"password": "",
					"options":  config.M{"sslmode": "disable"},
				},
			},
		},
	}
}

// WithPostgres has to leave the application pointed at the scratch database,
// not at whatever the project's .env happens to say. Getting this wrong means
// a test suite quietly writing into a developer's working database.
func TestWithPostgresCreatesAndMigratesAScratchDatabase(t *testing.T) {
	oldMigrator := migration.GetMigrator()
	t.Cleanup(func() { _ = oldMigrator })

	migration.GetMigrator().AddMigration(&migration.Migration{
		Version: "20260201000000",
		Up: func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE apptest_probe (id BIGSERIAL PRIMARY KEY, note TEXT)`)
			return err
		},
		Down: func(tx *sql.Tx) error {
			_, err := tx.Exec(`DROP TABLE apptest_probe`)
			return err
		},
	})

	cfg := postgresConfig()
	a := New(t, WithConfig(cfg), WithPostgres())

	// The configuration the application booted with now names the scratch
	// database, and it is not the placeholder.
	conn := cfg["sql"].(config.M)["connections"].(config.M)["postgres"].(config.M)
	name, _ := conn["database"].(string)
	if !strings.HasPrefix(name, "apptest_") {
		t.Fatalf("sql.connections.postgres.database = %q, want a scratch name", name)
	}
	if a.scratch == nil || a.scratch.name != name {
		t.Fatalf("scratch database = %+v, config says %q", a.scratch, name)
	}

	// The migration ran, in that database.
	db, err := sql.Open("postgres", dsnForDatabase(a.scratch.adminDSN, name))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var n int
	err = db.QueryRow(`SELECT count(*) FROM information_schema.tables
	                   WHERE table_schema = current_schema() AND table_name = 'apptest_probe'`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("migrations did not run against the scratch database")
	}
}

// Two applications in one package must not share a database, or tests see
// each other's rows and start depending on order.
func TestWithPostgresIsolatesEachApp(t *testing.T) {
	first := New(t, WithConfig(postgresConfig()), WithPostgresNoMigrate())
	second := New(t, WithConfig(postgresConfig()), WithPostgresNoMigrate())

	if first.scratch.name == second.scratch.name {
		t.Fatalf("both apps got %q", first.scratch.name)
	}
}

// pointConfigAt is the part most likely to be given a configuration it cannot
// use, and the failure has to name the problem rather than panic on a type
// assertion.
func TestPointConfigAtRejectsUnusableConfigurations(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.M
		want string
	}{
		{"no sql block", config.M{}, "pass WithConfig"},
		{"no connections", config.M{"sql": config.M{"default": "postgres"}}, "sql.connections is missing"},
		{"no default", config.M{"sql": config.M{"connections": config.M{}}}, "sql.default is not set"},
		{
			"default names a missing connection",
			config.M{"sql": config.M{"default": "pg", "connections": config.M{}}},
			"sql.connections.pg is missing",
		},
		{
			"default is not postgres",
			config.M{"sql": config.M{"default": "mysql", "connections": config.M{
				"mysql": config.M{"driver": "mysql"},
			}}},
			"needs the default connection to be postgres",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &fatalRecorder{}
			pointConfigAt(probe, tc.cfg, "host=h user=u dbname=d", "apptest_x")
			if probe.message == "" {
				t.Fatal("accepted a configuration it cannot use")
			}
			if !strings.Contains(probe.message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", probe.message, tc.want)
			}
		})
	}
}

func TestDSNForDatabaseReplacesOnlyTheName(t *testing.T) {
	got := parseDSN(dsnForDatabase("host=localhost port=5432 user=ada dbname=postgres sslmode=disable", "scratch"))
	if got["dbname"] != "scratch" {
		t.Errorf("dbname = %q", got["dbname"])
	}
	for key, want := range map[string]string{
		"host": "localhost", "port": "5432", "user": "ada", "sslmode": "disable",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

// fatalRecorder stands in for *testing.T so a failure path can be asserted
// without failing the test that asserts it.
type fatalRecorder struct{ message string }

func (f *fatalRecorder) Helper() {}
func (f *fatalRecorder) Fatalf(format string, args ...any) {
	if f.message == "" {
		f.message = sprintf(format, args...)
	}
}

// The drop is registered the instant the database exists, before anything
// that can fail. Everything after CREATE DATABASE calls t.Fatalf on failure,
// and Fatalf calls runtime.Goexit, so a cleanup registered later would never
// run — a developer iterating on a broken migration would accumulate one
// scratch database per attempt. (Observed: ten, before this was fixed.)
//
// The failure path itself cannot be driven through New, because New takes a
// *testing.T and its Fatalf would fail whichever test called it. So this
// asserts the two halves that can be checked: the teardown really drops, and
// it is safe to call more than once, which is what makes registering it early
// harmless.
func TestTearDownDropsTheDatabaseAndIsIdempotent(t *testing.T) {
	a := New(t, WithConfig(postgresConfig()), WithPostgresNoMigrate())

	name := a.scratch.name
	adminDSN := a.scratch.adminDSN
	if !databaseExists(t, adminDSN, name) {
		t.Fatalf("%s was not created", name)
	}

	a.tearDownPostgres()
	if databaseExists(t, adminDSN, name) {
		t.Errorf("%s survived teardown", name)
	}

	// Called again by t.Cleanup at the end of the test; it must not fail or
	// panic on an already-dropped database.
	a.tearDownPostgres()
}

func databaseExists(t *testing.T, adminDSN, name string) bool {
	t.Helper()
	db, err := sql.Open("postgres", adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}
