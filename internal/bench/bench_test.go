package bench

import "testing"

func TestSummarize(t *testing.T) {
	var s []float64
	for i := 100; i >= 1; i-- {
		s = append(s, float64(i))
	}
	got := Summarize(s)
	if got != (Stats{Min: 1, P50: 50, P95: 95, P99: 99, Max: 100}) {
		t.Fatalf("%+v", got)
	}
	if got := Summarize([]float64{7}); got.P99 != 7 || got.Min != 7 {
		t.Fatalf("%+v", got)
	}
}
