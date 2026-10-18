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
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ClaudeCode is Claude Code's name in the pins file and the store.
const ClaudeCode = "claude-code"

// Lavish is Lavish's name in the pins file and the store.
const Lavish = "lavish"

// Pins is the pins file: the provider versions this asmai runs. Claude Code
// and Lavish are pinned; Codex joins them in a later milestone.
type Pins struct {
	ClaudeCode Pin `json:"claude-code"`
	// Lavish is the Lavish build this asmai runs, when it pins one.
	Lavish *LavishPin `json:"lavish,omitempty"`
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
	if p.Lavish != nil {
		if err := p.Lavish.check(); err != nil {
			return Pins{}, fmt.Errorf("the pins file is not valid: %s: %w", Lavish, err)
		}
	}
	return p, nil
}

// LavishPin pins AsmAI's Lavish: the lavish-axi release and the Deno it is
// compiled with, which together name a Lavish build, and the digest of each
// platform's executable once the build is published (ADR 0010). Releases
// reuse a Lavish build until one of the two versions moves.
type LavishPin struct {
	// Version is the pinned lavish-axi release.
	Version string `json:"version"`
	// Deno is the Deno version Lavish is compiled with.
	Deno string `json:"deno"`
	// Releases is where Lavish builds are published: a Lavish build is the
	// release Releases/Tag serves, with an executable for each platform.
	Releases string `json:"releases"`
	// Platforms holds each certified platform's executable by the channel
	// name for the platform. A platform the Lavish build is not yet
	// published for is absent.
	Platforms map[string]Download `json:"platforms,omitempty"`
}

// Build names the Lavish build the pin selects, such as "0.1.89+deno2.9.6".
// It is the version the store records for the install.
func (p LavishPin) Build() string {
	return p.Version + "+deno" + p.Deno
}

// Tag is the tag of the release that publishes the Lavish build, such as
// "lavish-v0.1.89-deno2.9.6".
func (p LavishPin) Tag() string {
	return "lavish-v" + p.Version + "-deno" + p.Deno
}

// ExecutableName is the name of the Lavish build's executable for platform
// on its release.
func ExecutableName(platform string) string {
	return "lavish-" + platform
}

func (p LavishPin) check() error {
	if !versionPattern.MatchString(p.Version) {
		return fmt.Errorf("version %q is not a version", p.Version)
	}
	if !versionPattern.MatchString(p.Deno) {
		return fmt.Errorf("deno version %q is not a version", p.Deno)
	}
	u, err := url.Parse(p.Releases)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("releases %q is not an http or https URL", p.Releases)
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
	Title string
	// Source says where the download comes from, for people, such as
	// "Claude Code's official channel".
	Source   string
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
		Source:   "Claude Code's official channel",
		Version:  pin.Version,
		Platform: platform,
		URL:      u,
		Download: d,
		Path:     filepath.Join(dir, ClaudeCode, pin.Version, "claude"),
	}, nil
}

// ErrNoLavishBuild is wrapped by the error of planning a Lavish install that
// the pins file has no published build for: this asmai pins none, or the
// pinned Lavish build is not yet published for the platform.
var ErrNoLavishBuild = errors.New("no Lavish build is pinned")

// LavishPlan plans installing the pinned Lavish build for platform into dir,
// the state directory's providers directory, beside the pinned Claude Code.
// It fetches the executable from the Lavish build's release in talvor/asmai
// and keeps it at dir/lavish/BUILD/lavish. The error wraps ErrNoLavishBuild
// when the pins file pins no Lavish build for the platform.
func (p Pins) LavishPlan(platform, dir string) (Plan, error) {
	pin := p.Lavish
	if pin == nil {
		return Plan{}, fmt.Errorf("%w: the pins file pins no Lavish", ErrNoLavishBuild)
	}
	d, ok := pin.Platforms[platform]
	if !ok {
		return Plan{}, fmt.Errorf("%w: the Lavish build %s is not published for %s yet", ErrNoLavishBuild, pin.Tag(), platform)
	}
	u, err := url.JoinPath(pin.Releases, pin.Tag(), ExecutableName(platform))
	if err != nil {
		return Plan{}, err
	}
	return Plan{
		Name:     Lavish,
		Title:    "Lavish",
		Source:   "AsmAI's Lavish build, a release of talvor/asmai",
		Version:  pin.Build(),
		Platform: platform,
		URL:      u,
		Download: d,
		Path:     filepath.Join(dir, Lavish, pin.Build(), "lavish"),
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
	return n == plan.Download.Size && hex.EncodeToString(h.Sum(nil)) == plan.Download.SHA256 && Executable(plan.Path), nil
}

// How an installed provider stands against the pins file.
const (
	// StatePinned is the pinned version, and the file at its path is the
	// pinned download.
	StatePinned = "pinned"
	// StateSuperseded is an install of a version the pins file no longer
	// pins.
	StateSuperseded = "superseded"
	// StateMissing is the pinned version with no file at its path.
	StateMissing = "missing"
	// StateMismatch is the pinned version with a file at its path that is not
	// the pinned download: a copy that updated itself, or was replaced.
	StateMismatch = "mismatch"
)

// Standing says how an install of version stands against the pin plan is
// for: pinned, superseded by a newer pin, missing from plan.Path, or a
// mismatch with the pinned download. Only a pinned install is one AsmAI runs.
func (plan Plan) Standing(version string) (string, error) {
	if version != plan.Version {
		return StateSuperseded, nil
	}
	if _, err := os.Stat(plan.Path); errors.Is(err, os.ErrNotExist) {
		return StateMissing, nil
	}
	present, err := Present(plan)
	if err != nil {
		return "", err
	}
	if !present {
		return StateMismatch, nil
	}
	return StatePinned, nil
}

// Executable reports whether path names a regular file its owner can execute.
func Executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o100 != 0
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

// versionTimeout is how long a provider has to answer --version.
const versionTimeout = 30 * time.Second

// versionOutput is what Claude Code prints for --version: its version first,
// then its name, as in "2.1.292 (Claude Code)".
var versionOutput = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(\s|$)`)

// ClaudeCodeVersion runs the Claude Code at path with --version, and returns
// the version it reports. It is what the executable says it is, not what the
// store or the pins file recorded for the path: a provider that updated
// itself reports its new version.
func ClaudeCodeVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	// A version check must not start an update.
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s did not report its version within %s", path, versionTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("running %s --version: %w", path, err)
	}
	m := versionOutput.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", fmt.Errorf("%s --version printed %q, which does not start with a version", path, strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

// MarshalPins writes the pins file as it is committed: indented, with a
// final newline.
func MarshalPins(p Pins) ([]byte, error) {
	out, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}
