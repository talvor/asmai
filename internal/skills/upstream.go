// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// LatestReleaseURL returns the GitHub API address that names the latest
// release of the repository at source, such as
// https://github.com/mattpocock/skills.
func LatestReleaseURL(source string) (string, error) {
	u, err := url.Parse(source)
	if err != nil || u.Host != "github.com" {
		return "", fmt.Errorf("%q is not a GitHub repository", source)
	}
	owner, repo, ok := strings.Cut(strings.Trim(u.Path, "/"), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("%q is not a GitHub repository", source)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo), nil
}

// LatestRelease asks client for the tag of the latest release of upstream's
// repository at address, a LatestReleaseURL. It reads nothing else and
// changes nothing.
func LatestRelease(ctx context.Context, client *http.Client, address string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", address, resp.Status)
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("reading %s: %w", address, err)
	}
	if release.Tag == "" {
		return "", fmt.Errorf("%s names no release", address)
	}
	return release.Tag, nil
}

// Newer reports whether the release latest is newer than the release
// current, comparing dotted numbers such as v1.3.1. Releases that are not
// dotted numbers are newer when they differ.
func Newer(current, latest string) bool {
	if current == latest {
		return false
	}
	c, cok := releaseNumbers(current)
	l, lok := releaseNumbers(latest)
	if !cok || !lok {
		return true
	}
	for i := 0; i < max(len(c), len(l)); i++ {
		var a, b int
		if i < len(c) {
			a = c[i]
		}
		if i < len(l) {
			b = l[i]
		}
		if a != b {
			return b > a
		}
	}
	return false
}

func releaseNumbers(release string) ([]int, bool) {
	var numbers []int
	for _, part := range strings.Split(strings.TrimPrefix(release, "v"), ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		numbers = append(numbers, n)
	}
	return numbers, true
}
