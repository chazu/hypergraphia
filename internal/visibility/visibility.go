// Package visibility checks whether a git remote is publicly readable, so
// init can refuse to put team memories where anyone can fetch them.
package visibility

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Results.
const (
	Public  = "public"
	Private = "private"
	Unknown = "unknown" // non-GitHub host, or the check failed
)

var githubRe = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|(?:ssh://)?git@github\.com[:/])([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// GitHubRepo extracts owner and repo from a github.com remote URL.
func GitHubRepo(url string) (owner, repo string, ok bool) {
	m := githubRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Check asks GitHub, without credentials, whether the repository is visible
// to anyone. 200 means public; 404 means private (or absent). Anything else,
// and non-GitHub remotes, are Unknown.
func Check(ctx context.Context, client *http.Client, apiBase, url string) string {
	owner, repo, ok := GitHubRepo(url)
	if !ok {
		return Unknown
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/repos/"+owner+"/"+repo, nil)
	if err != nil {
		return Unknown
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hypergraphia")
	resp, err := client.Do(req)
	if err != nil {
		return Unknown
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return Public
	case http.StatusNotFound:
		return Private
	}
	return Unknown
}
