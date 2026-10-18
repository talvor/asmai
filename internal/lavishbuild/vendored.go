// SPDX-License-Identifier: Apache-2.0

package lavishbuild

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/talvor/asmai/internal/licenses"
)

// OFL is the SPDX identifier of the SIL Open Font License 1.1, which is
// accepted for the fonts Lavish vendors and for nothing else (ADR 0010).
const OFL = "OFL-1.1"

// Vendored holds the licenses of what lavish-axi vendors into its dist
// directory, as its own THIRD-PARTY-NOTICES.md lists them: the whiteboard
// bundle, the fonts and the design assets. The package's licenses cover its
// own code only.
type Vendored struct {
	// Entries are the table rows of the notices, by what is vendored.
	Entries []VendoredEntry
}

// VendoredEntry is one row of lavish-axi's notices: what is vendored and the
// license it says it is under.
type VendoredEntry struct {
	Section string
	What    string
	License string
	// ID is the SPDX identifier of License, or "" when it is not one that is
	// understood.
	ID string
}

// ParseVendored reads the tables of lavish-axi's THIRD-PARTY-NOTICES.md.
func ParseVendored(notices []byte) (Vendored, error) {
	var v Vendored
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(notices))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
			continue
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		what, license := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])
		if strings.Trim(what, "- ") == "" || strings.EqualFold(license, "License") {
			continue // the separator and the header
		}
		v.Entries = append(v.Entries, VendoredEntry{Section: section, What: what, License: license, ID: licenseID(license)})
	}
	if err := sc.Err(); err != nil {
		return Vendored{}, err
	}
	if len(v.Entries) == 0 {
		return Vendored{}, fmt.Errorf("lavish-axi's THIRD-PARTY-NOTICES.md lists nothing vendored, which is not the file this tool understands")
	}
	return v, nil
}

// licenseID returns the SPDX identifier a notices table's license cell names,
// or "" for a license it does not recognise.
func licenseID(cell string) string {
	switch {
	case strings.HasPrefix(cell, "SIL Open Font License 1.1"):
		return OFL
	case strings.HasPrefix(cell, "MIT"):
		return "MIT"
	case strings.HasPrefix(cell, "Apache-2.0"):
		return "Apache-2.0"
	case strings.HasPrefix(cell, "ISC"):
		return "ISC"
	case strings.HasPrefix(cell, "BSD-2-Clause"):
		return "BSD-2-Clause"
	case strings.HasPrefix(cell, "BSD-3-Clause"):
		return "BSD-3-Clause"
	}
	return ""
}

// Problems returns why each vendored thing's license is not acceptable: not
// on the allow-list, or not one that is understood. The SIL OFL 1.1 is
// acceptable for the fonts section only.
func (v Vendored) Problems() []string {
	var problems []string
	for _, e := range v.Entries {
		allowed := licenses.Allowed
		if strings.HasPrefix(e.Section, "Fonts") {
			allowed = append(slices.Clone(licenses.Allowed), OFL)
		}
		if e.ID == "" {
			problems = append(problems, fmt.Sprintf("%s (%s): license %q is not one that is understood", e.What, e.Section, e.License))
		} else if !slices.Contains(allowed, e.ID) {
			problems = append(problems, fmt.Sprintf("%s (%s): license %s is not on the allow-list", e.What, e.Section, e.ID))
		}
	}
	return problems
}

// UsesOFL reports whether any vendored font is under the SIL OFL 1.1, whose
// text the notices then carry.
func (v Vendored) UsesOFL() bool {
	return slices.ContainsFunc(v.Entries, func(e VendoredEntry) bool { return e.ID == OFL })
}
