// SPDX-License-Identifier: Apache-2.0

// Command app is compiled in the license check's tests.
package main

import (
	"example.com/mitlib"
	"example.com/unknown"
	"example.com/unlicensed"
)

func main() {
	mitlib.F()
	unknown.F()
	unlicensed.F()
}
