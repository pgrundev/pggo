// Package bench runs pgGo's tiny connectivity benchmark.
package bench

import (
	"context"
	"sort"
	"time"

	"github.com/pgrundev/pggo/internal/output"
	"github.com/pgrundev/pggo/internal/postgres"
)

// Stats summarizes a set of samples in milliseconds.
type Stats struct{ Min, P50, P95, P99, Max float64 }

// Summarize computes nearest-rank percentiles.
func Summarize(ms []float64) Stats {
	if len(ms) == 0 {
		return Stats{}
	}
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	pct := func(p float64) float64 {
		i := int(p/100*float64(len(s))+0.999999) - 1
		if i < 0 {
			i = 0
		}
		return s[i]
	}
	return Stats{Min: s[0], P50: pct(50), P95: pct(95), P99: pct(99), Max: s[len(s)-1]}
}

// JSON renders the stats as {"min":..,"p50":..,"p95":..,"p99":..,"max":..}.
func (s Stats) JSON() []byte {
	return output.NewObject().Ms("min", s.Min).Ms("p50", s.P50).Ms("p95", s.P95).Ms("p99", s.P99).Ms("max", s.Max).Bytes()
}

// Result of a benchmark run.
type Result struct {
	Connect Stats // new connection: dial + TLS + auth + startup
	Query   Stats // SELECT 1 round trip on a warm connection
}

func since(t time.Time) float64 { return float64(time.Since(t).Nanoseconds()) / 1e6 }

// Run opens n connections and runs n SELECT 1 round trips on a warm connection.
func Run(ctx context.Context, cfg *postgres.Config, n int) (*Result, error) {
	connect := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t := time.Now()
		c, err := postgres.Connect(ctx, cfg)
		if err != nil {
			return nil, err
		}
		connect = append(connect, since(t))
		c.Close()
	}
	c, err := postgres.Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	req := &postgres.Request{SQL: "SELECT 1", OnRow: func([][]byte) {}}
	if _, err := c.Run(ctx, req); err != nil { // warm-up
		return nil, err
	}
	query := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t := time.Now()
		if _, err := c.Run(ctx, req); err != nil {
			return nil, err
		}
		query = append(query, since(t))
	}
	return &Result{Connect: Summarize(connect), Query: Summarize(query)}, nil
}
