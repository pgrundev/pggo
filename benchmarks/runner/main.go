// Command runner benchmarks pggo against psql and a minimal pgx program.
//
//	go run ./runner -url 'postgres://...' -n 200 -warm 2000 > results.md
//
// It measures binary size, cold startup, connect + SELECT 1 (a fresh process
// per sample), warm SELECT 1 (one connection), and peak RSS, and prints a
// Markdown report with p50/p95/p99.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type stats struct{ p50, p95, p99, max float64 }

func summarize(s []float64) stats {
	sort.Float64s(s)
	p := func(q float64) float64 { return s[int(q/100*float64(len(s))+0.999999)-1] }
	return stats{p(50), p(95), p(99), s[len(s)-1]}
}

// sample runs argv once and returns wall time (ms) and peak RSS (MiB).
func sample(argv []string, stdin string) (float64, float64) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	t := time.Now()
	out, err := cmd.Output()
	ms := float64(time.Since(t).Nanoseconds()) / 1e6
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v failed: %v\n%s\n", argv, err, out)
		os.Exit(1)
	}
	rss := float64(cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss)
	if runtime.GOOS == "darwin" {
		rss /= 1024 // bytes on macOS, KiB on Linux
	}
	return ms, rss / 1024
}

func repeat(argv []string, n int) (stats, stats) {
	sample(argv, "") // warm the page cache
	ms := make([]float64, n)
	rss := make([]float64, n)
	for i := range ms {
		ms[i], rss[i] = sample(argv, "")
	}
	return summarize(ms), summarize(rss)
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func mb(n int64) string { return fmt.Sprintf("%.2f MB", float64(n)/1e6) }

func row(name string, s stats) string {
	return fmt.Sprintf("| %s | %.2f | %.2f | %.2f | %.2f |", name, s.p50, s.p95, s.p99, s.max)
}

var timingRE = regexp.MustCompile(`Time: ([0-9.]+) ms`)

func psqlWarm(psql, url string, n int) stats {
	var sb strings.Builder
	sb.WriteString("\\timing on\nSELECT 1;\n") // warm-up
	for i := 0; i < n; i++ {
		sb.WriteString("SELECT 1;\n")
	}
	cmd := exec.Command(psql, url, "-X", "-At", "-q")
	cmd.Stdin = strings.NewReader(sb.String())
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "psql warm:", err)
		os.Exit(1)
	}
	var ms []float64
	for _, m := range timingRE.FindAllStringSubmatch(string(out), -1) {
		v, _ := strconv.ParseFloat(m[1], 64)
		ms = append(ms, v)
	}
	return summarize(ms[1:])
}

func jsonWarm(argv []string, key string) stats {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v: %v %s\n", argv, err, out)
		os.Exit(1)
	}
	var m map[string]map[string]float64
	json.Unmarshal(out, &m)
	s := m[key]
	return stats{s["p50"], s["p95"], s["p99"], s["max"]}
}

func main() {
	url := flag.String("url", os.Getenv("DATABASE_URL"), "PostgreSQL URL")
	n := flag.Int("n", 200, "samples for per-process measurements")
	warm := flag.Int("warm", 2000, "samples for warm SELECT 1")
	pggo := flag.String("pggo", "../bin/pggo", "pggo binary")
	pgx := flag.String("pgxmin", "../bin/pgxmin", "pgxmin binary")
	psql := flag.String("psql", "psql", "psql binary")
	flag.Parse()
	if p, err := exec.LookPath(*psql); err == nil {
		*psql = p
	}
	*pggo, _ = filepath.Abs(*pggo)
	*pgx, _ = filepath.Abs(*pgx)

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	pr := func(f string, a ...any) { fmt.Fprintf(out, f+"\n", a...) }

	ver, _ := exec.Command(*pggo, "info", *url).Output()
	psqlVer, _ := exec.Command(*psql, "--version").Output()
	pr("# pgGo benchmark\n")
	pr("- Date: %s", time.Now().UTC().Format(time.RFC3339))
	pr("- Host: %s/%s, %d CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	pr("- Server: `%s`", strings.TrimSpace(string(ver)))
	pr("- psql: `%s`", strings.TrimSpace(string(psqlVer)))
	pr("- Samples: %d per-process runs, %d warm queries. Times in ms, RSS in MiB.\n", *n, *warm)

	pr("## Binary size\n")
	pr("| tool | size |\n|---|---|")
	pr("| pggo (stripped) | %s |", mb(fileSize(*pggo)))
	pr("| pgxmin (pgx v5, stripped) | %s |", mb(fileSize(*pgx)))
	libpq := ""
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(*psql), "..", "lib", "libpq.*.dylib")); len(matches) > 0 {
		libpq = fmt.Sprintf(" + libpq %s", mb(fileSize(matches[0])))
	} else if matches, _ := filepath.Glob("/usr/lib/*/libpq.so.5"); len(matches) > 0 {
		libpq = fmt.Sprintf(" + libpq %s", mb(fileSize(matches[0])))
	}
	pr("| psql | %s%s (dynamically linked) |\n", mb(fileSize(*psql)), libpq)

	type tool struct {
		name             string
		startup, connect []string
	}
	tools := []tool{
		{"pggo", []string{*pggo, "version"}, []string{*pggo, "query", *url, "SELECT 1"}},
		{"pgxmin", []string{*pgx, "-version"}, []string{*pgx, *url}},
		{"psql", []string{*psql, "--version"}, []string{*psql, *url, "-X", "-Atc", "SELECT 1"}},
	}

	hdr := "| tool | p50 | p95 | p99 | max |\n|---|---|---|---|---|"
	pr("## Cold startup (process start → exit, no database)\n\n%s", hdr)
	for _, t := range tools {
		s, _ := repeat(t.startup, *n)
		pr(row(t.name, s))
	}
	pr("\n## Connect + SELECT 1 (new process per sample)\n\n%s", hdr)
	rss := map[string]stats{}
	for _, t := range tools {
		s, r := repeat(t.connect, *n)
		rss[t.name] = r
		pr(row(t.name, s))
	}
	pr("\n## Peak RSS during connect + SELECT 1 (MiB)\n\n%s", hdr)
	for _, t := range tools {
		pr(row(t.name, rss[t.name]))
	}
	pr("\n## Warm SELECT 1 round trip (one connection)\n\n%s", hdr)
	pr(row("pggo (`pggo bench`)", jsonWarm([]string{*pggo, "bench", *url, "--iterations", strconv.Itoa(*warm), "--timeout", "10m"}, "select1_ms")))
	pr(row("pgxmin", jsonWarm([]string{*pgx, "-warm", strconv.Itoa(*warm), *url}, "select1_ms")))
	pr(row("psql (`\\timing`)", psqlWarm(*psql, *url, *warm)))
	pr("\nNotes: `pggo query` wraps the statement in `BEGIN READ ONLY … ROLLBACK`, pipelined in the same round trip.")
	pr("pggo's warm number comes from `pggo bench`, which uses the same code path as `query` minus the read-only wrapper.")
}
