package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

type connector struct {
	driver     driver.Driver
	dsn        string
	preemptive []string
}

var _ driver.Connector = &connector{}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}

	for _, query := range c.preemptive {
		if err := c.settle(ctx, conn, query); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	return conn, nil
}

// settleAttempts and settleBackoff bound how long a setup statement is
// retried.
//
// Switching a database into WAL needs exclusive access to it, and SQLite
// reports that conflict straight away rather than waiting it out the way
// busy_timeout covers ordinary contention. A connection opened while another
// one happens to be writing therefore fails for a reason that clears itself in
// milliseconds — during startup, when several processes open the same new
// database and immediately migrate it, that is not a rare coincidence.
//
// Only the journal mode is retried. Every other pragma either applies or is
// wrong, and retrying a wrong one would delay the error it is going to report
// anyway, once per connection the pool opens.
const (
	settleAttempts = 5
	settleBackoff  = 5 * time.Millisecond
)

func (c *connector) settle(ctx context.Context, conn driver.Conn, query string) error {
	attempts := 1
	if strings.Contains(query, "journal_mode") {
		attempts = settleAttempts
	}

	backoff := settleBackoff

	var err error
	for attempt := range attempts {
		if attempt > 0 {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(err, ctx.Err())
			case <-timer.C:
			}

			backoff *= 2
		}

		if err = c.exec(ctx, conn, query); err == nil {
			return nil
		}
	}

	return err
}

func (c *connector) Driver() driver.Driver {
	return c.driver
}

// open returns a pool whose every new connection runs the preemptive
// statements before it is handed out, so pragmas apply to the whole pool
// rather than to whichever connection happened to serve the setup query.
func open(driverName string, dataSourceName string, preemptive ...string) (*sql.DB, error) {
	db, err := sql.Open(driverName, "")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	return openDriver(db.Driver(), dataSourceName, preemptive...), nil
}

// openDriver is open for a driver that is already in hand, which is how
// migration reaches the driver of the database it is migrating without being
// told its name again.
func openDriver(drv driver.Driver, dataSourceName string, preemptive ...string) *sql.DB {
	return sql.OpenDB(&connector{
		driver:     drv,
		dsn:        dataSourceName,
		preemptive: preemptive,
	})
}

func (c *connector) exec(ctx context.Context, conn driver.Conn, query string) error {
	var (
		stmt driver.Stmt
		err  error
	)

	if cpc, ok := conn.(driver.ConnPrepareContext); ok {
		stmt, err = cpc.PrepareContext(ctx, query)
	} else {
		stmt, err = conn.Prepare(query)
	}

	if err != nil {
		return fmt.Errorf("prepare preemptive %q: %w", query, err)
	}
	defer stmt.Close()

	if sec, ok := stmt.(driver.StmtExecContext); ok {
		_, err = sec.ExecContext(ctx, nil)
	} else {
		//nolint:staticcheck // fallback for driver.Stmt implementations without context support
		_, err = stmt.Exec(nil)
	}

	if err != nil {
		return fmt.Errorf("execute preemptive %q: %w", query, err)
	}

	return nil
}
