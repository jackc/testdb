[![Go Reference](https://pkg.go.dev/badge/github.com/jackc/testdb.svg)](https://pkg.go.dev/github.com/jackc/testdb)

# testdb

testdb is a Go package that manages access to multiple PostgreSQL databases with to enable parallel testing.

Integration tests often need to run with exclusive access to a database. i.e. Multiple tests cannot use the same
database at the same time.

In addition, when an application has multiple packages that each have integration tests, then running `go test ./...`
will run the package tests in parallel by default.

testdb uses the PostgreSQL server to manage exclusive access to databases even when the tests are running in parallel in
different processes.

## Preparing the Databases

testdb requires that the testdb management data be loaded and test databases already be created and prepared before the
it can be used..

See `setup_test_databases.sql` for an example setup script.

## Example Usage

Typically, a package should have one global manager. This test manager should be created in `TestMain()`.

```go
var TestDBManager *testdb.Manager

func TestMain(m *testing.M) {
	TestDBManager = testutil.InitTestDBManager(m)
	os.Exit(m.Run())
}
```

To allow for creating test managers in multiple packages the actual creation of the test manager is in contained in a function in a separate package. e.g.

```go
// InitTestDBManager performs the standard initialization of a *testdb.Manager. It requires a *testing.M to
// ensure it is only called by TestMain. If something fails it calls os.Exit(1).
func InitTestDBManager(*testing.M) *testdb.Manager {
	manager := &testdb.Manager{
		AfterAcquireDB: func(t testing.TB, db *testdb.DB, conn *pgx.Conn) {
			// Do some sort of setup or initial testing.

			t.Cleanup(func() {
				// Do some cleanup or final testing.
			})
		},
		ResetDB: func(ctx context.Context, conn *pgx.Conn) error {
			// Do whatever is necessary to restore the database to pristine condition
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	err := manager.Connect(ctx, fmt.Sprintf("dbname=%s", os.Getenv("TEST_DATABASE")))
	if err != nil {
		fmt.Println("failed to init testdb.Manager:", err)
		os.Exit(1)
	}

	return manager
}
```

The actual test could be something like this:

```go
func TestSomethingWithExclusiveDatabaseAccess(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	conn := TestDBManager.AcquireDB(t, ctx).Connect(t, ctx)

	// Run tests with conn. No defer / cleanup is necessary as testdb calls t.Cleanup internally.
}
```

## Reset / Cleanup Database

testdb does not provide any functionality to reset or cleanup the database between tests. That must be provided in the
`ResetDB` hook by the caller.

https://github.com/jackc/pgundolog may be useful to provide this functionality in an application-agnostic manner.
