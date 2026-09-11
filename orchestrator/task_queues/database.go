package taskqueues

import (
	"context"
	"database/sql"
	"strings"
)

// A validated namespace lets consumers host independent protocols in one database.
type Database struct {
	*sql.DB
	Prefix string
}
type Tx struct {
	*sql.Tx
	Prefix string
}

func (d *Database) query(q string) string { return strings.ReplaceAll(q, "am_", d.Prefix) }
func (d *Database) ExecContext(c context.Context, q string, a ...any) (sql.Result, error) {
	return d.DB.ExecContext(c, d.query(q), a...)
}
func (d *Database) QueryContext(c context.Context, q string, a ...any) (*sql.Rows, error) {
	return d.DB.QueryContext(c, d.query(q), a...)
}
func (d *Database) QueryRowContext(c context.Context, q string, a ...any) *sql.Row {
	return d.DB.QueryRowContext(c, d.query(q), a...)
}
func (d *Database) BeginTx(c context.Context, o *sql.TxOptions) (*Tx, error) {
	t, e := d.DB.BeginTx(c, o)
	if e != nil {
		return nil, e
	}
	return &Tx{t, d.Prefix}, nil
}
func (t *Tx) ExecContext(c context.Context, q string, a ...any) (sql.Result, error) {
	return t.Tx.ExecContext(c, strings.ReplaceAll(q, "am_", t.Prefix), a...)
}
func (t *Tx) QueryRowContext(c context.Context, q string, a ...any) *sql.Row {
	return t.Tx.QueryRowContext(c, strings.ReplaceAll(q, "am_", t.Prefix), a...)
}
