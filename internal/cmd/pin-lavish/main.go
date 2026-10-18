// SPDX-License-Identifier: Apache-2.0

// Command pin-lavish records the digest and size of each platform's Lavish
// executable in the pins file, once the Lavish build workflow has made them
// (10 rule 10). The workflow runs it on the executables it published and
// proposes the result as a pull request; `asmai providers install` then
// checks every download against these digests. Run it from the repository
// root:
//
//	go run ./internal/cmd/pin-lavish -dir build
//
// where build holds lavish-linux-x64 and lavish-darwin-arm64, the names the
// executables have on the Lavish build's release.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/talvor/asmai/internal/providers"
)

func main() {
	pins := flag.String("pins", "pins.json", "the pins file to update")
	dir := flag.String("dir", "", "the directory holding each platform's executable, named as on the release")
	flag.Parse()
	if err := run(*pins, *dir); err != nil {
		fmt.Fprintln(os.Stderr, "pin-lavish:", err)
		os.Exit(1)
	}
}

func run(pinsFile, dir string) error {
	if dir == "" {
		return fmt.Errorf("-dir is needed")
	}
	data, err := os.ReadFile(pinsFile)
	if err != nil {
		return err
	}
	pins, err := providers.ParsePins(data)
	if err != nil {
		return err
	}
	if pins.Lavish == nil {
		return fmt.Errorf("%s pins no Lavish", pinsFile)
	}
	built := map[string]providers.Download{}
	// Every platform the pins file certifies has a Lavish build.
	for platform := range pins.ClaudeCode.Platforms {
		exe, err := os.ReadFile(filepath.Join(dir, providers.ExecutableName(platform)))
		if err != nil {
			return fmt.Errorf("the Lavish build has no executable for the certified platform %s: %w", platform, err)
		}
		sum := sha256.Sum256(exe)
		built[platform] = providers.Download{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(exe))}
	}
	pins.Lavish.Platforms = built
	out, err := providers.MarshalPins(pins)
	if err != nil {
		return err
	}
	return os.WriteFile(pinsFile, out, 0o644)
}
