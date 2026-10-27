package simhash

import (
	"testing"

	"github.com/chazu/hypergraphia/internal/tokens"
)

func fp(s string) uint64 { return Of(tokens.Words(s)) }

func TestNearDuplicates(t *testing.T) {
	a := "The integration tests need a running Postgres container; start it with make db-up before running go test ./... in the store package."
	b := "The integration tests need a running Postgres container; start it with make db-up before running go test ./... in the store package!"
	c := "The integration tests need a running postgres container, start it with make db-up before you run go test ./... in the store package."
	d := "Release builds are signed with the ad-hoc codesign identity on macOS; Linux builds are not signed at all."
	if Distance(fp(a), fp(b)) > NearDistance {
		t.Errorf("punctuation-only change should be near: %d", Distance(fp(a), fp(b)))
	}
	if dist := Distance(fp(a), fp(c)); dist > 12 {
		t.Errorf("light paraphrase should be close: %d", dist)
	}
	if Distance(fp(a), fp(d)) <= NearDistance {
		t.Errorf("unrelated texts should be far: %d", Distance(fp(a), fp(d)))
	}
}

func TestShortTexts(t *testing.T) {
	if Of(nil) != 0 {
		t.Error("empty input should fingerprint to 0")
	}
	if Of([]string{"wal"}) == 0 {
		t.Error("single word should still fingerprint")
	}
}
