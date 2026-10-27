package tokens

import (
	"reflect"
	"testing"
)

func TestSplitIdent(t *testing.T) {
	cases := map[string][]string{
		"ParseHTTPConfig":              {"parse", "http", "config"},
		"max_retry-count":              {"max", "retry", "count"},
		"internal/syncer/transport.go": {"internal", "syncer", "transport", "go"},
		"http2Transport":               {"http", "2", "transport"},
		"getURLs":                      {"get", "ur", "ls"}, // acronym+plural is ambiguous; stable is what matters
		"simple":                       {"simple"},
	}
	for in, want := range cases {
		if got := SplitIdent(in); !reflect.DeepEqual(got, want) {
			t.Errorf("SplitIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsCodeLike(t *testing.T) {
	yes := []string{"ParseHTTPConfig", "max_retry", "internal/x.go", "config.yaml", "http2", "fooBar"}
	no := []string{"config", "Config", "HTTP", "timeout", "2026"}
	for _, s := range yes {
		if !IsCodeLike(s) {
			t.Errorf("%q should be code-like", s)
		}
	}
	for _, s := range no {
		if IsCodeLike(s) {
			t.Errorf("%q should not be code-like", s)
		}
	}
}

func TestRawKeepsIdentifiersAndPaths(t *testing.T) {
	got := Raw("Edit `internal/config/parse.go` (ParseHTTPConfig), then run make.")
	want := []string{"Edit", "internal/config/parse.go", "ParseHTTPConfig", "then", "run", "make"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Raw = %q", got)
	}
}

func TestIdents(t *testing.T) {
	got := Idents("ParseHTTPConfig panics", "internal/config")
	if got != "parse http config internal config" {
		t.Errorf("Idents = %q", got)
	}
}

func TestWordsAndStopwords(t *testing.T) {
	got := Words("Don't use the WAL-mode, please!")
	want := []string{"don", "t", "use", "the", "wal", "mode", "please"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Words = %q", got)
	}
	if !IsStopword("the") || IsStopword("wal") {
		t.Error("stopword set wrong")
	}
}

func TestAnalyze(t *testing.T) {
	got := Analyze("Caching configs: ParseHTTPConfig in internal/config")
	want := map[string]bool{"cach": true, "config": true, "parsehttpconfig": true, "pars": true, "http": true, "intern": true, "internal/config": false}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for w, should := range want {
		if set[w] != should {
			t.Errorf("Analyze: term %q present=%v, want %v (got %q)", w, set[w], should, got)
		}
	}
	if Stem("caching") != Stem("cached") {
		t.Error("caching and cached should share a stem")
	}
}
