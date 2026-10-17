// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewerComparesReleaseNumbers(t *testing.T) {
	for _, c := range []struct {
		current, latest string
		want            bool
	}{
		{"v1.3.1", "v1.3.1", false},
		{"v1.3.1", "v1.4.0", true},
		{"v1.3.1", "v1.3.2", true},
		{"v1.3.1", "v2.0.0", true},
		{"v1.3.1", "v1.3.0", false},
		{"v1.9.0", "v1.10.0", true},
		{"v1.3", "v1.3.1", true},
		{"v1.3.1", "nightly", false},
		{"nightly", "v1.4.0", false},
	} {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestLatestReleaseURLNamesTheGitHubAPI(t *testing.T) {
	got, err := LatestReleaseURL("https://github.com/mattpocock/skills")
	if err != nil || got != "https://api.github.com/repos/mattpocock/skills/releases/latest" {
		t.Errorf("LatestReleaseURL = %q, %v", got, err)
	}
	if _, err := LatestReleaseURL("https://example.com/a/b"); err == nil {
		t.Error("a source outside GitHub was accepted")
	}
}

func TestLatestReleaseReadsTheTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name": "v1.4.0"}`))
	}))
	defer server.Close()
	got, err := LatestRelease(context.Background(), server.Client(), server.URL+"/latest")
	if err != nil || got != "v1.4.0" {
		t.Errorf("LatestRelease = %q, %v", got, err)
	}
	if _, err := LatestRelease(context.Background(), server.Client(), server.URL+"/missing"); err == nil {
		t.Error("a missing release was read without an error")
	}
}
