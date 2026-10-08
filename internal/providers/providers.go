// SPDX-License-Identifier: Apache-2.0

// Package providers installs AsmAI's own copies of the provider CLIs its
// agents run: the versions the pins file names, fetched from each provider's
// official channel and kept in the state directory, apart from the user's own
// installations. AsmAI never redistributes a provider; it only fetches what
// the provider publishes, and checks it against the pins file.
package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

// ClaudeCode is Claude Code's name in the pins file and the store.
const ClaudeCode = "claude-code"

// Pins is the pins file: the provider versions this asmai runs. M1 pins
// Claude Code; Codex and Lavish join it in later milestones.
type Pins struct {
	ClaudeCode Pin `json:"claude-code"`
}

// Pin is one pinned provider version.
type Pin struct {
	// Version is the pinned version.
	Version string `json:"version"`
	// Channel is the provider's official release channel, which serves the
	// version's download for each platform at Channel/VERSION/PLATFORM/NAME.
	Channel string `json:"channel"`
	// Platforms holds each certified platform's download, by the channel's
	// name for the platform.
	Platforms map[string]Download `json:"platforms"`
}

// Download is what a pin requires of one platform's download.
type Download struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var (
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParsePins parses a pins file, refusing one with a field it does not know
// or a pin it could not install from.
func ParsePins(data []byte) (Pins, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Pins
	if err := dec.Decode(&p); err != nil {
		return Pins{}, fmt.Errorf("the pins file is not valid: %w", err)
	}
	if err := p.ClaudeCode.check(); err != nil {
		return Pins{}, fmt.Errorf("the pins file is not valid: %s: %w", ClaudeCode, err)
	}
	return p, nil
}

func (p Pin) check() error {
	if !versionPattern.MatchString(p.Version) {
		return fmt.Errorf("version %q is not a version", p.Version)
	}
	u, err := url.Parse(p.Channel)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("channel %q is not an http or https URL", p.Channel)
	}
	if len(p.Platforms) == 0 {
		return errors.New("no platforms")
	}
	for platform, d := range p.Platforms {
		if !sha256Pattern.MatchString(d.SHA256) {
			return fmt.Errorf("%s: sha256 %q is not a SHA-256 in lower-case hex", platform, d.SHA256)
		}
		if d.Size <= 0 {
			return fmt.Errorf("%s: size %d is not a size", platform, d.Size)
		}
	}
	return nil
}

// Platform returns the name Claude Code's channel gives the platform GOOS
// and GOARCH name, or false for a platform it publishes nothing for.
func Platform(goos, goarch string) (string, bool) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[goarch]
	if arch == "" || (goos != "linux" && goos != "darwin") {
		return "", false
	}
	return goos + "-" + arch, true
}

// Plan is one provider install: what is fetched, from where, how it is
// checked, and where AsmAI keeps it.
type Plan struct {
	// Name is the provider's name in the pins file and the store.
	Name string
	// Title is the provider's name for people.
	Title    string
	Version  string
	Platform string
	// URL is the download on the provider's official channel.
	URL      string
	Download Download
	// Path is where AsmAI keeps the provider's executable.
	Path string
}

// ClaudeCodePlan plans installing the pinned Claude Code for platform into
// dir, the state directory's providers directory.
func (p Pins) ClaudeCodePlan(platform, dir string) (Plan, error) {
	pin := p.ClaudeCode
	d, ok := pin.Platforms[platform]
	if !ok {
		return Plan{}, fmt.Errorf("the pins file pins no Claude Code for %s", platform)
	}
	u, err := url.JoinPath(pin.Channel, pin.Version, platform, "claude")
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Name:     ClaudeCode,
		Title:    "Claude Code",
		Version:  pin.Version,
		Platform: platform,
		URL:      u,
		Download: d,
		Path:     filepath.Join(dir, ClaudeCode, pin.Version, "claude"),
	}, nil
}

// Present reports whether plan.Path already holds the pinned download.
func Present(plan Plan) (bool, error) {
	f, err := os.Open(plan.Path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return false, err
	}
	return n == plan.Download.Size && hex.EncodeToString(h.Sum(nil)) == plan.Download.SHA256, nil
}

// Fetch downloads plan.URL with client, checks its size and SHA-256 against
// the pin, and makes it plan.Path's executable. A download that fails or does
// not match leaves nothing at plan.Path.
func Fetch(ctx context.Context, client *http.Client, plan Plan) (err error) {
	dir := filepath.Dir(plan.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	part, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return err
	}
	defer func() {
		part.Close()
		if err != nil {
			os.Remove(part.Name())
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, plan.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", plan.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching %s: the channel answered %s", plan.URL, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(part, h), io.LimitReader(resp.Body, plan.Download.Size+1))
	if err != nil {
		return fmt.Errorf("fetching %s: %w", plan.URL, err)
	}
	if n != plan.Download.Size {
		return fmt.Errorf("fetching %s: the download is %d bytes, but the pins file says %d", plan.URL, n, plan.Download.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != plan.Download.SHA256 {
		return fmt.Errorf("fetching %s: the download's SHA-256 is %s, but the pins file says %s", plan.URL, got, plan.Download.SHA256)
	}
	if err := part.Chmod(0o755); err != nil {
		return err
	}
	if err := part.Sync(); err != nil {
		return err
	}
	if err := part.Close(); err != nil {
		return err
	}
	return os.Rename(part.Name(), plan.Path)
}
