package zdb

// This file contains the public API and all documentation.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// DB is an interface to the database; this can be a regular connection, a
// transaction, or a wrapped connection to add features such as logging.
//
// If this is not a transaction, then Commit() and Rollback() will always return
// an error. If this is a transaction, then Begin() is a no-op, and Close() will
// rollback the transaction and close the database connection.
//
// See documentation on the top-level functions for more details on the methods.
type DB interface {
	DBSQL() *sql.DB
	Driver() DriverType
	Ping(context.Context) error
	Version(context.Context) (Version, error)

	Prepare(ctx context.Context, query string, params ...interface{}) (string, []interface{}, error)
	Load(ctx context.Context, name string) (string, error)

	Exec(ctx context.Context, query string, params ...interface{}) error
	NumRows(ctx context.Context, query string, params ...interface{}) (int64, error)
	InsertID(ctx context.Context, idColumn, query string, params ...interface{}) (int64, error)
	Get(ctx context.Context, dest interface{}, query string, params ...interface{}) error
	Select(ctx context.Context, dest interface{}, query string, params ...interface{}) error
	Query(ctx context.Context, query string, params ...interface{}) (*Rows, error)

	BindNamed(query string, param interface{}) (newquery string, params []interface{}, err error)
	Rebind(query string) string
	DriverName() string
	Close() error

	TX(context.Context, func(context.Context) error) error
	Begin(context.Context, ...beginOpt) (context.Context, DB, error)
	Rollback() error
	Commit() error
}

type (
	// P ("params") is an alias for map[string]interface{}, just because it's
	// less typing and looks less noisy 🙃
	P map[string]interface{}

	// L ("list") is an alias for []interface.
	L []interface{}

	// SQL represents a safe SQL string that will be directly inserted in the
	// query without any modification, rather than passed as a parameter.
	//
	// Use with wisdom! Careless use of this can open you to SQL injections.
	// Generally speaking you rarely want to use this, except in some rare cases
	// where 1) parameters won't work, and 2) you're really really sure this
	// value is safe.
	SQL string

	// DriverType is the SQL driver.
	DriverType uint8
)

func (d DriverType) String() string {
	switch d {
	case DriverSQLite:
		return "SQLite"
	case DriverPostgreSQL:
		return "PostgreSQL"
	case DriverMariaDB:
		return "MariaDB"
	default:
		return "(unknown)"
	}
}

var (
	DriverUnknown    DriverType = 0
	DriverSQLite     DriverType = 1
	DriverPostgreSQL DriverType = 2
	DriverMariaDB    DriverType = 3
)

// ErrTransactionStarted is returned when a transaction is already started; this
// can often be treated as a non-fatal error.
var ErrTransactionStarted = errors.New("transaction already started")

// Prepare a query for sendoff to the database.
//
// Named parameters (:name) are used if params contains a map or struct;
// positional parameters (? or $1) are used if it's doesn't. You can add
// multiple structs or maps, but mixing named and positional parameters is not
// allowed.
//
// Everything between {{:name ..}} is parsed as a conditional; for example
// {{:foo query}} will only be added if "foo" from params is true or not a zero
// type. Conditionals only work with named parameters.
//
// If the query starts with "load:" then it's loaded from the filesystem or
// embedded files; see Load() for details.
//
// Additional DumpArgs can be added to dump the results of the query to stderr
// for testing and debugging:
//
//    DumpQuery      Show the query
//    DumpExplain    Show query plain (WILL RUN QUERY TWICE!)
//    DumpResult     Show the query result (WILL RUN QUERY TWICE!)
//    DumpVertical   Show results in vertical format.
//
// Running the query twice for a select is usually safe (just slower), but
// running insert, update, or delete twice may cause problems.
func Prepare(ctx context.Context, query string, params ...interface{}) (string, []interface{}, error) {
	return prepareImpl(ctx, MustGetDB(ctx), query, params...)
}

// Load a query from the filesystem or embeded files.
//
// Queries are loaded from "db/query/{name}-{driver}.sql" or
// "db/query/{name}.sql". Every query will have the file name inserted in the
// first line; for example with "db/query/select-x.sql":
//
//   select x from y;
//
// Then the actual query will be:
//
//   /* select-x */
//   select x from y;
//
// This allows identifying queries in logging and statistics such as
// pg_stat_statements.
//
// Typical usage with Query() is to use "load:name", instead of calling this
// directly:
//
//   zdb.QueryGet(ctx, "load:select-x", &foo, zdb.P{
//       "param": "foo",
//   })
func Load(ctx context.Context, name string) (string, error) {
	return loadImpl(ctx, MustGetDB(ctx), name)
}

// Begin a new transaction.
//
// The returned context is a copy of the original with the DB replaced with a
// transaction. The same transaction is also returned directly.
//
// Nested transactions return the original transaction together with
// ErrTransactionStarted (which is not a fatal error).
func Begin(ctx context.Context, opts ...beginOpt) (context.Context, DB, error) {
	return beginImpl(ctx, MustGetDB(ctx))
}

type beginOpt func(*sql.TxOptions)

func BeginReadOnly() beginOpt { return func(o *sql.TxOptions) { o.ReadOnly = true } }
func BeginIsolation(level sql.IsolationLevel) beginOpt {
	return func(o *sql.TxOptions) { o.Isolation = level }
}

// TX runs the given function in a transaction.
//
// The context passed to the callback has the DB replaced with a transaction.
// The transaction is committed if the fn returns nil, or will be rolled back if
// it's not.
//
// Multiple TX() calls can be nested, but they all run the same transaction and
// are comitted only if the outermost transaction returns true.
//
// This is just a more convenient wrapper for Begin().
func TX(ctx context.Context, fn func(context.Context) error) error {
	return txImpl(ctx, MustGetDB(ctx), fn)
}

// Exec executes a query without returning the result.
//
// This uses Prepare(), and all the documentation from there applies here too.
func Exec(ctx context.Context, query string, params ...interface{}) error {
	return execImpl(ctx, MustGetDB(ctx), query, params...)
}

// NumRows executes a query and returns the number of affected rows.
//
// This uses Prepare(), and all the documentation from there applies here too.
func NumRows(ctx context.Context, query string, params ...interface{}) (int64, error) {
	return numRowsImpl(ctx, MustGetDB(ctx), query, params...)
}

// InsertID runs a INSERT query and returns the ID column idColumn.
//
// If multiple rows are inserted it will return the ID of the last inserted row.
// This works for both PostgreSQL and SQLite.
//
// This uses Prepare(), and all the documentation from there applies here too.
func InsertID(ctx context.Context, idColumn, query string, params ...interface{}) (int64, error) {
	return insertIDImpl(ctx, MustGetDB(ctx), idColumn, query, params...)
}

// Select one or more rows; dest needs to be a pointer to a slice.
//
// Returns nil if there are no rows.
//
// This uses Prepare(), and all the documentation from there applies here too.
func Select(ctx context.Context, dest interface{}, query string, params ...interface{}) error {
	return selectImpl(ctx, MustGetDB(ctx), dest, query, params...)
}

// Get one row, returning sql.ErrNoRows if there are no rows.
//
// This uses Prepare(), and all the documentation from there applies here too.
func Get(ctx context.Context, dest interface{}, query string, params ...interface{}) error {
	return getImpl(ctx, MustGetDB(ctx), dest, query, params...)
}

// Query the database without immediately loading the result.
//
// This gives more flexibility over Select(), and won't load the entire result
// in memory and allows fetching the result one row at a time.
//
// This won't return an error if there are no rows.
//
// This uses Prepare(), and all the documentation from there applies here too.
func Query(ctx context.Context, query string, params ...interface{}) (*Rows, error) {
	return queryImpl(ctx, MustGetDB(ctx), query, params...)
}

type Rows struct{ r *sqlx.Rows }

func (r *Rows) Next() bool                              { return r.r.Next() }
func (r *Rows) Err() error                              { return r.r.Err() }
func (r *Rows) Close() error                            { return r.r.Close() }
func (r *Rows) Columns() ([]string, error)              { return r.r.Columns() }
func (r *Rows) ColumnTypes() ([]*sql.ColumnType, error) { return r.r.ColumnTypes() }
func (r *Rows) Scan(dest ...interface{}) error {
	if len(dest) > 1 {
		return r.r.Scan(dest...)
	}

	d := dest[0]
	if m, ok := d.(*map[string]interface{}); ok {
		if *m == nil {
			*m = make(map[string]interface{})
		}
		return r.r.MapScan(*m)
	}
	if s, ok := d.(*[]interface{}); ok {
		s2, err := r.r.SliceScan()
		if err != nil {
			return err
		}
		*s = s2
		return nil
	}
	return r.r.StructScan(d)
}

// func (r *Rows) Scan(s ...interface{}) error        { return r.r.Scan(s...) }
// func (r *Rows) Struct(d interface{}) error         { return r.r.StructScan(d) }
// func (r *Rows) Slice() ([]interface{}, error)      { return r.r.SliceScan() }
// func (r *Rows) Map(d map[string]interface{}) error { return r.r.MapScan(d) }

// WithDB returns a copy of the context with the DB instance.
func WithDB(ctx context.Context, db DB) context.Context {
	return context.WithValue(ctx, ctxkey, db)
}

// GetDB gets the DB from the context.
func GetDB(ctx context.Context) (DB, bool) {
	db, ok := ctx.Value(ctxkey).(DB)
	return db, ok
}

// MustGet gets the DB from the context, panicking if there is none.
func MustGetDB(ctx context.Context) DB {
	db, ok := GetDB(ctx)
	if !ok {
		panic(fmt.Sprintf("zdb.MustGetDB: no DB on this context (value: %#v)", db))
	}
	return db
}

// Unwrap this database, removing any of the zdb wrappers and returning the
// underlying sqlx.DB or sqlx.Tx.
//
// To wrap a zdb.DB object embed the zdb.DB interface, which contains the parent
// DB connection.
//
// The Unwrap() method is expected to return the parent DB.
//
// Then implement override whatever you want; usually you will want to implement
// the dbImpl interface, which contains the methods that actually interact with
// the database. All the DB methods call this under the hood. This way you don't
// have to wrap all the methods on DB, but just five.
//
// In Begin() you will want to return a new wrapped DB instance with the
// transaction attached.
func Unwrap(db DB) DB {
	uw, ok := db.(interface {
		Unwrap() DB
	})
	if !ok {
		return db
	}
	return Unwrap(uw.Unwrap())
}

// ErrNoRows reports if this error is sql.ErrNoRows.
func ErrNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// Driver gets the SQL driver.
func Driver(ctx context.Context) DriverType { return MustGetDB(ctx).Driver() }
