package apptest

import "testing"

// lib/pq accepts a DSN in two shapes and this package used to understand one.
//
// A URL was not rejected, it was mangled: strings.Fields on
// "postgres://you@host:5432/db?sslmode=disable" yields a single field, Cut on
// "=" splits it at the query parameter, and what came back was a settings map
// keyed on the whole URL up to "sslmode". The error named the symptom and hid
// the cause — pq: unsupported sslmode "disable dbname=apptest_20a89edc8248" —
// which is a confusing thing to be told when the DSN you supplied was
// perfectly good.
//
// It matters because URL form is what anybody writes in a CI workflow, and
// because it was the form that found this.
func TestADSNIsUnderstoodInEitherForm(t *testing.T) {
	want := map[string]string{
		"host":    "db.internal",
		"port":    "5433",
		"user":    "ada",
		"dbname":  "ledger",
		"sslmode": "disable",
	}

	for name, dsn := range map[string]string{
		"keyword":                        "host=db.internal port=5433 user=ada dbname=ledger sslmode=disable",
		"url":                            "postgres://ada@db.internal:5433/ledger?sslmode=disable",
		"url with the postgresql scheme": "postgresql://ada@db.internal:5433/ledger?sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			got := parseDSN(dsn)
			for key, value := range want {
				if got[key] != value {
					t.Errorf("%s = %q, want %q (whole parse: %v)", key, got[key], value, got)
				}
			}
		})
	}
}

// A password is the one setting that cannot be reconstructed if it is dropped,
// and url.Userinfo keeps it separately from the username.
func TestAPasswordSurvivesAURLDSN(t *testing.T) {
	got := parseDSN("postgres://ada:s3cret@db.internal:5432/ledger")

	if got["user"] != "ada" {
		t.Errorf("user = %q, want ada", got["user"])
	}
	if got["password"] != "s3cret" {
		t.Errorf("password = %q, want s3cret", got["password"])
	}
}

// The scratch database's DSN is derived from the administrative one, so a URL
// in has to produce something lib/pq can open — naming the new database and
// keeping every other setting, sslmode above all, since getting that wrong is
// what surfaced the bug.
func TestTheScratchDSNKeepsTheRestOfTheSettings(t *testing.T) {
	got := parseDSN(dsnForDatabase(
		"postgres://ada@db.internal:5433/postgres?sslmode=disable", "scratch_1"))

	if got["dbname"] != "scratch_1" {
		t.Errorf("dbname = %q, want scratch_1", got["dbname"])
	}
	if got["sslmode"] != "disable" {
		t.Errorf("sslmode = %q, want disable — dropping it sends lib/pq to its "+
			"own default of require", got["sslmode"])
	}
	if got["host"] != "db.internal" || got["port"] != "5433" || got["user"] != "ada" {
		t.Errorf("the connection details did not survive: %v", got)
	}
}
