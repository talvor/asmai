// SPDX-License-Identifier: Apache-2.0

// Package spdx checks that AsmAI's source files carry the Apache-2.0 SPDX
// license line.
package spdx

import (
	"bufio"
	"io/fs"
	"path"
	"strings"
)

// Line is the license identifier every source file starts with, after its
// comment marker.
const Line = "SPDX-License-Identifier: Apache-2.0"

// commentMarkers maps each source file extension to its line comment marker.
var commentMarkers = map[string]string{
	".go":   "//",
	".sh":   "#",
	".yml":  "#",
	".yaml": "#",
}

// skippedDirs are never searched: git's own files and local build output at
// the root of fsys.
var skippedDirs = map[string]bool{
	".git": true,
	"dist": true,
}

// Missing returns, in lexical order, the source files in fsys that do not
// start with Line. A script's first line may be its shebang, with Line on the
// next.
func Missing(fsys fs.FS) ([]string, error) {
	var missing []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skippedDirs[name] {
				return fs.SkipDir
			}
			return nil
		}
		marker, ok := commentMarkers[path.Ext(name)]
		if !ok {
			return nil
		}
		ok, err = startsWithLine(fsys, name, marker)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, name)
		}
		return nil
	})
	return missing, err
}

func startsWithLine(fsys fs.FS, name, marker string) (bool, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	want := marker + " " + Line
	lines := bufio.NewScanner(f)
	for i := 0; i < 2 && lines.Scan(); i++ {
		line := strings.TrimRight(lines.Text(), " \t\r")
		if line == want {
			return true, nil
		}
		if i > 0 || !strings.HasPrefix(line, "#!") {
			break
		}
	}
	return false, lines.Err()
}
