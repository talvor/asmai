// SPDX-License-Identifier: Apache-2.0

// Package dashboard will hold the Dashboard (ADR 0015), built on Bubble Tea
// and Lip Gloss.
package dashboard

// Stand-in imports so go mod tidy keeps Bubble Tea and Lip Gloss as module
// dependencies until the Dashboard (issue #112) uses them for real. Remove
// them once it does.
import (
	_ "charm.land/bubbletea/v2"
	_ "charm.land/lipgloss/v2"
)
