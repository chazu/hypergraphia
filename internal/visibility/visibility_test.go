package visibility

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubRepo(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:chazu/litelog.git":            "chazu/litelog",
		"https://github.com/chazu/litelog":            "chazu/litelog",
		"https://user:tok@github.com/chazu/lite.log/": "chazu/lite.log",
		"ssh://git@github.com/chazu/litelog.git":      "chazu/litelog",
		"https://gitlab.com/chazu/litelog.git":        "",
		"file:///tmp/remote.git":                      "",
	} {
		o, r, ok := GitHubRepo(url)
		got := ""
		if ok {
			got = o + "/" + r
		}
		if got != want {
			t.Errorf("%s → %q, want %q", url, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/public":
			w.WriteHeader(200)
		case "/repos/o/private":
			w.WriteHeader(404)
		default:
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for repo, want := range map[string]string{"public": Public, "private": Private, "limited": Unknown} {
		if got := Check(ctx, srv.Client(), srv.URL, "git@github.com:o/"+repo+".git"); got != want {
			t.Errorf("%s: %s, want %s", repo, got, want)
		}
	}
	if Check(ctx, nil, "", "file:///x") != Unknown {
		t.Error("non-github should be unknown")
	}
}
