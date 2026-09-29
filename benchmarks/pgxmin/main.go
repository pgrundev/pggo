// Command pgxmin is the smallest reasonable pgx program, used as a baseline.
//
//	pgxmin -version          print version and exit (startup cost)
//	pgxmin URL               connect, SELECT 1, print JSON
//	pgxmin -warm N URL       connect once, time N SELECT 1 round trips
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "-version" {
		fmt.Println(`{"ok":true,"version":"pgxmin"}`)
		return
	}
	warm := 0
	if len(args) == 3 && args[0] == "-warm" {
		warm, _ = strconv.Atoi(args[1])
		args = args[2:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, args[0])
	if err != nil {
		fmt.Printf("{\"ok\":false,\"error\":%q}\n", err.Error())
		os.Exit(1)
	}
	defer conn.Close(ctx)
	var one int
	if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		fmt.Printf("{\"ok\":false,\"error\":%q}\n", err.Error())
		os.Exit(1)
	}
	if warm == 0 {
		fmt.Printf("{\"ok\":true,\"rows\":[{\"?column?\":%d}]}\n", one)
		return
	}
	ms := make([]float64, warm)
	for i := range ms {
		t := time.Now()
		if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			os.Exit(1)
		}
		ms[i] = float64(time.Since(t).Nanoseconds()) / 1e6
	}
	sort.Float64s(ms)
	p := func(q float64) float64 { return ms[int(q/100*float64(len(ms))+0.999999)-1] }
	fmt.Printf("{\"ok\":true,\"select1_ms\":{\"min\":%.3f,\"p50\":%.3f,\"p95\":%.3f,\"p99\":%.3f,\"max\":%.3f}}\n", ms[0], p(50), p(95), p(99), ms[len(ms)-1])
}
