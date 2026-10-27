package secrets

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestEntropyRates guards the entropy thresholds statistically: random tokens
// must almost always be caught, and real identifiers almost never.
func TestEntropyRates(t *testing.T) {
	s := New(Options{})
	for _, n := range []int{16, 20, 24, 32, 40, 64} {
		caught := 0
		const trials = 300
		for i := 0; i < trials; i++ {
			if _, ok := s.judge("", randFrom(alnum, n), 0); ok {
				caught++
			}
		}
		rate := float64(caught) / trials
		// 16-char tokens are borderline by nature (gitleaks' keyword rules
		// cover labelled ones); from 20 chars the pass must be reliable.
		min := 0.93
		if n < 20 {
			min = 0.70
		}
		t.Logf("len %d: caught %.1f%%", n, 100*rate)
		if rate < min {
			t.Errorf("len %d: detection %.2f < %.2f", n, rate, min)
		}
	}

	ident := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{15,}`)
	seen := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(runtime.GOROOT(), "src", "net", "http", "*.go"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, id := range ident.FindAllString(string(b), -1) {
			seen[id] = true
		}
	}
	if len(seen) < 200 {
		t.Skipf("only %d identifiers found in GOROOT", len(seen))
	}
	var fps []string
	for id := range seen {
		if _, ok := s.judge("", id, 0); ok {
			fps = append(fps, id)
		}
	}
	rate := float64(len(fps)) / float64(len(seen))
	t.Logf("identifiers: %d, false positives %d (%.2f%%): %v", len(seen), len(fps), 100*rate, fps)
	if rate > 0.01 {
		t.Errorf("false-positive rate %.3f > 0.01", rate)
	}
}
