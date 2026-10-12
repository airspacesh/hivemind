package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
)

// maxCachedStatements bounds the statements a connection keeps. The
// store runs about a hundred fixed SQL texts, and its listings and
// feeds build one more for each combination of filters asked for.
const maxCachedStatements = 256

// sqliteConn is what cachingConn passes through to a connection of
// modernc.org/sqlite.
type sqliteConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// cachingConnector opens SQLite connections that keep the statements
// they prepare. It is a database/sql/driver.Connector, for
// database/sql.OpenDB. A *sql.Stmt is no substitute, since preparing
// one takes a connection from the pool, which inside a transaction
// waits on the connection the transaction holds.
type cachingConnector struct {
	dsn string
}

func (c cachingConnector) Driver() driver.Driver {
	return &sqlite.Driver{}
}

func (c cachingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	inner, ok := conn.(sqliteConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("SQLite connection %T lacks a method", conn)
	}
	return &cachingConn{
		sqliteConn: inner, idle: map[string]driver.Stmt{},
	}, nil
}

// cachingConn runs each SQL text through a statement it prepared for
// that text before, so SQLite compiles the text once per connection
// rather than on every call. database/sql makes one call at a time on a
// connection, closing rows included, so it needs no lock.
type cachingConn struct {
	sqliteConn
	// idle holds the prepared statements no query is running, by their
	// SQL text. A statement leaves it while it runs, so a query that
	// runs its text again before the first is done prepares another.
	idle map[string]driver.Stmt
}

func (c *cachingConn) take(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if stmt, ok := c.idle[query]; ok {
		delete(c.idle, query)
		return stmt, nil
	}
	return c.PrepareContext(ctx, query)
}

func (c *cachingConn) closeIdle() error {
	var errs []error
	for _, stmt := range c.idle {
		errs = append(errs, stmt.Close())
	}
	clear(c.idle)
	return errors.Join(errs...)
}

// keep caches stmt, which has finished running query. A full cache
// starts over, so the texts in use now take the place of old ones.
func (c *cachingConn) keep(query string, stmt driver.Stmt) error {
	if _, ok := c.idle[query]; ok {
		return stmt.Close()
	}
	if len(c.idle) >= maxCachedStatements {
		if err := c.closeIdle(); err != nil {
			return errors.Join(err, stmt.Close())
		}
	}
	c.idle[query] = stmt
	return nil
}

func (c *cachingConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	stmt, err := c.take(ctx, query)
	if err != nil {
		return nil, err
	}
	result, err := stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	return result, errors.Join(err, c.keep(query, stmt))
}

func (c *cachingConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	stmt, err := c.take(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, errors.Join(err, c.keep(query, stmt))
	}
	return &cachedRows{Rows: rows, conn: c, query: query, stmt: stmt}, nil
}

func (c *cachingConn) Close() error {
	return errors.Join(c.closeIdle(), c.sqliteConn.Close())
}

// cachedRows are the rows of a cached statement, which return it to the
// cache when they close.
type cachedRows struct {
	driver.Rows
	conn  *cachingConn
	query string
	stmt  driver.Stmt
}

func (r *cachedRows) Close() error {
	return errors.Join(r.Rows.Close(), r.conn.keep(r.query, r.stmt))
}
