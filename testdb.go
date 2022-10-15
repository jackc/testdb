package testdb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager manages access to test databases. Connect or ConnectConfig must be called before any other methods.
type Manager struct {
	acquireMutex sync.Mutex
	acquireConn  *pgx.Conn

	releaseMutex sync.Mutex
	releaseConn  *pgx.Conn

	maintConnConfig *pgx.ConnConfig
	dbmaintConns    map[string]*pgx.Conn

	// AfterAcquireDB is called after a DB is acquired. It can be used to do setup such as loading fixtures or canaries.
	// t.Cleanup can be used to register cleanup or final tests such as checking canary health. conn is the maintenance
	// connection to db.
	AfterAcquireDB func(t testing.TB, db *DB, conn *pgx.Conn)

	// MakeConnConfig is used by DBs created by this Manager to configure connections. It returns a *pgx.ConnConfig for
	// suitable for connecting to connConfig.Database. connConfig is what would have been used if MakeConnConfig was nil.
	// connConfig may be modified and returned or an entirely new *pgx.ConnConfig may be created.
	//
	// connConfig is a copy of the *pgx.ConnConfig used to establish the manager connection, but with the Database field
	// replaced with the actual database to be connected to and with a OnNotice handler that logs PostgreSQL notices to
	MakeConnConfig func(t testing.TB, connConfig *pgx.ConnConfig) *pgx.ConnConfig

	// AfterConnect is called by DB.Connect after a connection is established. It is also used by any pool returned from
	// DB.ConnectPool. If an error is returned the connection will be closed.
	AfterConnect func(ctx context.Context, conn *pgx.Conn) error
}

// Connect establishes m's connection to the database with a connection string.
func (m *Manager) Connect(ctx context.Context, connStr string) error {
	connConfig, err := pgx.ParseConfig(connStr)
	if err != nil {
		return err
	}
	return m.ConnectConfig(ctx, connConfig)
}

// ConnectConfig establishes m's connection to the database with a connection config.
func (m *Manager) ConnectConfig(ctx context.Context, connConfig *pgx.ConnConfig) error {
	if m.acquireConn != nil {
		return errors.New("already connected")
	}

	if ctx == context.Background() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Second*15)
		defer cancel()
	}

	m.maintConnConfig = connConfig

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return err
	}

	_, err = conn.Exec(ctx, `listen testdb`)
	if err != nil {
		conn.Close(ctx)
		return err
	}

	m.acquireConn = conn

	m.releaseConn, err = pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return err
	}
	m.dbmaintConns = map[string]*pgx.Conn{}

	return nil
}

// AcquireDB acquires a DB for exclusive use. A t.Cleanup function is registered to release the database at the end of
// the test so no explicit cleanup is required.
func (m *Manager) AcquireDB(t testing.TB, ctx context.Context) *DB {
	m.acquireMutex.Lock()
	defer m.acquireMutex.Unlock()

	db := &DB{
		m: m,
	}

	for db.dbname == "" {
		func() {
			tx, err := m.acquireConn.Begin(ctx)
			if err != nil {
				t.Fatalf("failed to begin transaction: %v", err)
			}
			defer tx.Rollback(ctx)

			// The select from pg_stat_activity must only be done once in the transaction as the results are repeated. This
			// caused a problem when the tx was held for the entire loop as a database acquired by a pid that started after
			// this transaction would be found.
			err = tx.QueryRow(ctx, `select name from testdb.databases where acquirer_pid is null or not exists (select 1 from pg_stat_activity where pid = acquirer_pid) limit 1 for update skip locked`).Scan(&db.dbname)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("failed to select database name for acquisition: %v", err)
				}
				return
			}

			commandTag, err := tx.Exec(ctx, `update testdb.databases set acquirer_pid = pg_backend_pid() where name = $1`, db.dbname)
			if err != nil {
				t.Fatalf("acquire database failed: %v", err)
			}
			if commandTag.RowsAffected() != 1 {
				t.Fatalf("expected to acquire one database but got %d", commandTag.RowsAffected())
			}

			if _, ok := m.dbmaintConns[db.dbname]; !ok {
				maintConn, err := m.maintConnConnect(ctx, db.dbname)
				if err != nil {
					t.Fatalf("establish maintenance connection failed: %v", err)
				}
				m.dbmaintConns[db.dbname] = maintConn
			}

			err = db.reset(ctx)
			if err != nil {
				t.Fatalf("failed to reset database: %v", err)
			}

			err = tx.Commit(ctx)
			if err != nil {
				t.Fatalf("transaction commit of database acquisition failed: %v", err)
			}
		}()

		if db.dbname == "" {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			_, err := m.acquireConn.WaitForNotification(ctx)
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed to wait for notification of available database: %v", err)
			}
		}
	}

	t.Cleanup(func() { m.releaseDB(t, db) })

	t.Logf("AcquireDB database: %s, %v", db.DBName(), time.Now())

	if m.AfterAcquireDB != nil {
		m.AfterAcquireDB(t, db, m.dbmaintConns[db.dbname])
	}

	return db
}

func (m *Manager) releaseDB(t testing.TB, db *DB) {
	m.releaseMutex.Lock()
	defer m.releaseMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	commandTag, err := m.releaseConn.Exec(ctx, `update testdb.databases set acquirer_pid = null where name = $1`, db.dbname)
	if err != nil {
		t.Fatalf("failed to release database: %v", err)
	}
	if commandTag.RowsAffected() != 1 {
		t.Fatalf("expected to release one database but got %d", commandTag.RowsAffected())
	}

	t.Logf("releaseDB database: %s, %v", db.DBName(), time.Now())

	_, err = m.releaseConn.Exec(ctx, `notify testdb`)
	if err != nil {
		t.Fatalf("failed to notify release of database: %v", err)
	}
}

// maintConnConnect establishes a single connection to dbname.
func (m *Manager) maintConnConnect(ctx context.Context, dbname string) (*pgx.Conn, error) {
	config := m.maintConnConfig.Copy()
	config.Database = dbname
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}

	return conn, err
}

func (m *Manager) doMakeConnConfig(t testing.TB, dbname string) *pgx.ConnConfig {
	connConfig := m.maintConnConfig.Copy()
	connConfig.Database = dbname
	connConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		t.Logf("PostgreSQL %s: %s", n.Severity, n.Message)
	}

	if m.MakeConnConfig != nil {
		return m.MakeConnConfig(t, connConfig)
	}

	return connConfig
}

// DB represents exclusive access to a database.
type DB struct {
	m      *Manager
	dbname string
}

func (db *DB) DBName() string {
	return db.dbname
}

// Connect establishes a single connection to db. If the test only needs a single connection and does not have any
// transactional behavior of its own then prefer TxConnect as it may be faster. A t.Cleanup function is registered to
// close the connection at the end of the test so no explicit cleanup is required.
func (db *DB) Connect(t testing.TB, ctx context.Context) *pgx.Conn {
	config := db.m.doMakeConnConfig(t, db.dbname)
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn.Close(ctx)
	})

	if db.m.AfterConnect != nil {
		err := db.m.AfterConnect(ctx, conn)
		if err != nil {
			t.Fatal(err)
		}
	}

	return conn
}

// PoolConnect establishes a connection pool to db.  A t.Cleanup function is registered to release the pool at the
// end of the test so no explicit cleanup is required.
func (db *DB) PoolConnect(t testing.TB, ctx context.Context) *pgxpool.Pool {
	config, err := pgxpool.ParseConfig("")
	if err != nil {
		t.Fatal(err)
	}

	config.ConnConfig = db.m.doMakeConnConfig(t, db.dbname)
	config.AfterConnect = db.m.AfterConnect

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	return pool
}

// TxConnect establishes a single connection to db and starts a transaction. With pgundolog the transaction is
// unnecesary but tests may run faster when run in a transaction due to writes not needing to actually be durable until
// commit (which never will happen). A t.Cleanup function is registered to close the connection at the end of the test
// so no explicit cleanup is required.
func (db *DB) TxConnect(t testing.TB, ctx context.Context) pgx.Tx {
	conn := db.Connect(t, ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Purposely not rolling back transaction as it will happen implicitly when closing the connection.

	return tx
}

// reset resets the database.
func (db *DB) reset(ctx context.Context) error {
	_, err := db.m.dbmaintConns[db.dbname].Exec(ctx, `select pgundolog.undo()`)
	return err
}
