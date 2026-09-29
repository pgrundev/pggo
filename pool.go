package pggo

import (
	"context"
	"errors"
	"sync"
	"time"
)

// PoolConfig configures a Pool. The pool is deliberately minimal: a bounded
// set of connections, lazily opened, with per-connection setup and teardown
// hooks and a maximum lifetime.
type PoolConfig struct {
	Config          *Config
	MaxConns        int           // default 4
	MaxConnLifetime time.Duration // 0 = unlimited
	// AfterConnect runs on every new connection before first use; an error
	// discards the connection and fails the Acquire.
	AfterConnect func(ctx context.Context, c *Conn) error
	// BeforeClose runs before the pool closes a connection.
	BeforeClose func(c *Conn)
}

// Pool shares connections between goroutines.
type Pool struct {
	cfg    PoolConfig
	sem    chan struct{} // one token per connection slot
	mu     sync.Mutex
	idle   []*pooled
	closed bool
}

type pooled struct {
	c       *Conn
	created time.Time
}

// NewPool creates a pool. No connection is opened until the first Acquire.
func NewPool(cfg PoolConfig) *Pool {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 4
	}
	return &Pool{cfg: cfg, sem: make(chan struct{}, cfg.MaxConns)}
}

// Config returns a copy of the connection configuration the pool dials with.
func (p *Pool) Config() *Config { return p.cfg.Config.Copy() }

// PoolConn is a connection checked out of a Pool.
type PoolConn struct {
	p        *Pool
	pc       *pooled
	released bool
}

// Conn returns the underlying connection.
func (pc *PoolConn) Conn() *Conn { return pc.pc.c }

// Release returns the connection to the pool. A connection that is closed,
// broken, still inside a transaction, or past its lifetime is closed instead.
func (pc *PoolConn) Release() {
	if pc.released {
		return
	}
	pc.released = true
	pc.p.put(pc.pc)
}

// Acquire checks out a connection, opening one if needed. It blocks while
// MaxConns connections are in use.
func (p *Pool) Acquire(ctx context.Context) (*PoolConn, error) {
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.sem
		return nil, errors.New("pggo: pool is closed")
	}
	for len(p.idle) > 0 {
		pc := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if p.expired(pc) || pc.c.IsClosed() {
			p.mu.Unlock()
			p.destroy(pc)
			p.mu.Lock()
			continue
		}
		p.mu.Unlock()
		return &PoolConn{p: p, pc: pc}, nil
	}
	p.mu.Unlock()

	c, err := ConnectConfig(ctx, p.cfg.Config)
	if err != nil {
		<-p.sem
		return nil, err
	}
	if p.cfg.AfterConnect != nil {
		if err := p.cfg.AfterConnect(ctx, c); err != nil {
			c.Close()
			<-p.sem
			return nil, err
		}
	}
	return &PoolConn{p: p, pc: &pooled{c: c, created: time.Now()}}, nil
}

func (p *Pool) expired(pc *pooled) bool {
	return p.cfg.MaxConnLifetime > 0 && time.Since(pc.created) > p.cfg.MaxConnLifetime
}

func (p *Pool) put(pc *pooled) {
	defer func() { <-p.sem }()
	c := pc.c
	reusable := !c.IsClosed() && !c.busy && c.txStatus == 'I' && len(c.pending) == 0 && !p.expired(pc)
	p.mu.Lock()
	if reusable && !p.closed {
		p.idle = append(p.idle, pc)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.destroy(pc)
}

func (p *Pool) destroy(pc *pooled) {
	if p.cfg.BeforeClose != nil {
		p.cfg.BeforeClose(pc.c)
	}
	pc.c.Close()
}

// Close closes idle connections and marks the pool closed; connections in use
// are closed when released.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, pc := range idle {
		p.destroy(pc)
	}
}

// Query acquires a connection for the lifetime of the returned Rows.
func (p *Pool) Query(ctx context.Context, sql string, args ...any) (*Rows, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := pc.Conn().Query(ctx, sql, args...)
	if err != nil {
		pc.Release()
		return nil, err
	}
	rows.onClose = pc.Release
	return rows, nil
}

// QueryRow is QueryRow on a pooled connection.
func (p *Pool) QueryRow(ctx context.Context, sql string, args ...any) *Row {
	rows, err := p.Query(ctx, sql, args...)
	return &Row{rows: rows, err: err}
}

// Exec is Exec on a pooled connection.
func (p *Pool) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer pc.Release()
	return pc.Conn().Exec(ctx, sql, args...)
}

// BeginTx starts a transaction on a pooled connection, which returns to the
// pool when the transaction commits or rolls back.
func (p *Pool) BeginTx(ctx context.Context, opts TxOptions) (*Tx, error) {
	pc, err := p.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := pc.Conn().BeginTx(ctx, opts)
	if err != nil {
		pc.Release()
		return nil, err
	}
	tx.release = pc.Release
	return tx, nil
}
