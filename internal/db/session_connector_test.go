package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

// fakeConn records the statements sessionConnector applies to it.
type fakeConn struct {
	driver.Conn
	execs  []string
	failOn string
	closed bool
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.execs = append(c.execs, query)
	if c.failOn != "" && query == c.failOn {
		return nil, errors.New("setting rejected")
	}
	return driver.RowsAffected(0), nil
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

// plainConn has no ExecContext, like a driver that predates database/sql's
// context interfaces.
type plainConn struct {
	driver.Conn
	closed bool
}

func (c *plainConn) Close() error {
	c.closed = true
	return nil
}

type fakeConnector struct {
	driver.Connector
	conn driver.Conn
	err  error
}

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return c.conn, c.err
}

func TestSessionConnectorAppliesSettingsToEveryConnection(t *testing.T) {
	conn := &fakeConn{}
	sc := sessionConnector{Connector: fakeConnector{conn: conn}, settings: []string{"SET jit = off", "SET x = 1"}}

	got, err := sc.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got != conn {
		t.Fatalf("Connect returned a different conn")
	}
	if strings.Join(conn.execs, ";") != "SET jit = off;SET x = 1" {
		t.Fatalf("applied statements = %v", conn.execs)
	}
	if conn.closed {
		t.Fatal("a healthy connection must not be closed")
	}
}

func TestSessionConnectorDefaultsDisableJIT(t *testing.T) {
	conn := &fakeConn{}
	sc := sessionConnector{Connector: fakeConnector{conn: conn}, settings: sessionSettings}
	if _, err := sc.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(conn.execs) != 1 || conn.execs[0] != "SET jit = off" {
		t.Fatalf("default session settings = %v, want only SET jit = off", conn.execs)
	}
}

func TestSessionConnectorFailures(t *testing.T) {
	t.Run("connector error passes through", func(t *testing.T) {
		want := errors.New("dial failed")
		sc := sessionConnector{Connector: fakeConnector{err: want}, settings: sessionSettings}
		if _, err := sc.Connect(context.Background()); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
	})

	t.Run("a rejected setting closes the connection", func(t *testing.T) {
		conn := &fakeConn{failOn: "SET x = 1"}
		sc := sessionConnector{Connector: fakeConnector{conn: conn}, settings: []string{"SET jit = off", "SET x = 1"}}
		_, err := sc.Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), `"SET x = 1"`) {
			t.Fatalf("err = %v, want the rejected statement named", err)
		}
		if !conn.closed {
			t.Fatal("the connection must be closed when a setting fails")
		}
	})

	t.Run("a driver without ExecContext is refused", func(t *testing.T) {
		conn := &plainConn{}
		sc := sessionConnector{Connector: fakeConnector{conn: conn}, settings: sessionSettings}
		if _, err := sc.Connect(context.Background()); err == nil {
			t.Fatal("expected an error for a driver without ExecContext")
		}
		if !conn.closed {
			t.Fatal("the unusable connection must be closed")
		}
	})
}
