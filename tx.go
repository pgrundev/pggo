package pggo

import (
	"context"
	"errors"
)

// TxOptions configures BeginTx.
type TxOptions struct {
	ReadOnly bool // BEGIN READ ONLY: PostgreSQL rejects any write (SQLSTATE 25006)
}

// Tx is a transaction on one connection.
//
// BEGIN is not sent on its own: it is pipelined with the transaction's first
// statement, in the same protocol sync, so a transaction costs no extra round
// trip and its first statement can never run outside it (if BEGIN fails, the
// server skips the statement).
type Tx struct {
	c      *Conn
	begin  string
	closed bool
	// release runs once, after Commit/Rollback (used by Pool).
	release func()
}

// BeginTx starts a transaction.
func (c *Conn) BeginTx(ctx context.Context, opts TxOptions) (*Tx, error) {
	if err := c.usable(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	begin := "BEGIN"
	if opts.ReadOnly {
		begin = "BEGIN READ ONLY"
	}
	c.pending = append(c.pending, begin)
	return &Tx{c: c, begin: begin}, nil
}

// Begin starts a read-write transaction.
func (c *Conn) Begin(ctx context.Context) (*Tx, error) { return c.BeginTx(ctx, TxOptions{}) }

// Conn returns the underlying connection.
func (tx *Tx) Conn() *Conn { return tx.c }

func (tx *Tx) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	if tx.closed {
		return nil, ErrTxClosed
	}
	return tx.c.Query(ctx, sql, args...)
}

func (tx *Tx) QueryRow(ctx context.Context, sql string, args ...any) *Row {
	if tx.closed {
		return &Row{err: ErrTxClosed}
	}
	return tx.c.QueryRow(ctx, sql, args...)
}

func (tx *Tx) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	if tx.closed {
		return "", ErrTxClosed
	}
	return tx.c.Exec(ctx, sql, args...)
}

// unstarted reports (and clears) a BEGIN that never reached the server.
func (tx *Tx) unstarted() bool {
	p := tx.c.pending
	if len(p) > 0 && p[len(p)-1] == tx.begin {
		tx.c.pending = p[:len(p)-1]
		return true
	}
	return false
}

func (tx *Tx) end(ctx context.Context, sql string) (CommandTag, error) {
	if tx.closed {
		return "", ErrTxClosed
	}
	tx.closed = true
	defer func() {
		if tx.release != nil {
			tx.release()
		}
	}()
	if tx.unstarted() {
		return "", nil // nothing was sent, so there is nothing to end
	}
	return tx.c.Exec(ctx, sql)
}

// Commit commits the transaction. If the transaction had already failed, the
// server rolls it back and Commit returns ErrTxCommitRollback.
func (tx *Tx) Commit(ctx context.Context) error {
	tag, err := tx.end(ctx, "COMMIT")
	if err == nil && tag == "ROLLBACK" {
		return ErrTxCommitRollback
	}
	return err
}

// Rollback aborts the transaction. Calling it after Commit returns ErrTxClosed.
func (tx *Tx) Rollback(ctx context.Context) error {
	_, err := tx.end(ctx, "ROLLBACK")
	if errors.Is(err, ErrConnClosed) {
		return nil // the server rolls back a closed session's transaction itself
	}
	return err
}
