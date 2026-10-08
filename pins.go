// SPDX-License-Identifier: Apache-2.0

package asmai

import _ "embed"

// Pins is pins.json, the pins file: the provider versions this asmai runs,
// where each is fetched from, and the SHA-256 of each certified platform's
// download.
//
//go:embed pins.json
var Pins []byte
