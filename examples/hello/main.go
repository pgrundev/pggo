// Command hello connects to PostgreSQL with pggo and runs SELECT 1.
//
//	DATABASE_URL=postgres://user:pass@localhost:5432/db \
//	    go run github.com/pgrundev/pggo/examples/hello@latest
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pgrundev/pggo"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fail("set DATABASE_URL, e.g. DATABASE_URL=postgres://user:pass@localhost:5432/db")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := pggo.Connect(ctx, url)
	if err != nil {
		fail(err.Error())
	}
	defer conn.Close()
	fmt.Println("✓ connected to PostgreSQL", conn.ServerVersion())

	var n int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&n); err != nil {
		fail(err.Error())
	}
	fmt.Println("✓ SELECT 1 →", n)
	fmt.Printf("✓ %.1f ms\n", float64(time.Since(start).Microseconds())/1000)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "✗", msg)
	os.Exit(1)
}
