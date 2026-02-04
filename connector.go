package sqlitex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
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
		if err := c.exec(ctx, conn, query); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	return conn, nil
}

func (c *connector) Driver() driver.Driver {
	return c.driver
}

func Open(driverName string, dataSourceName string, preemptive ...string) (*sql.DB, error) {
	db, err := sql.Open(driverName, "")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	connector := &connector{
		driver:     db.Driver(),
		dsn:        dataSourceName,
		preemptive: preemptive,
	}

	return sql.OpenDB(connector), nil
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
		_, err = stmt.Exec(nil)
	}

	if err != nil {
		return fmt.Errorf("execute preemptive %q: %w", query, err)
	}

	return nil
}
