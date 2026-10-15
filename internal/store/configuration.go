// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"time"
)

// AppliedConfiguration is what a configuration.applied journal entry
// holds: the configuration file the user applied, and what it changed.
type AppliedConfiguration struct {
	// File is the configuration file's path and SHA256 its content's digest.
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	// Configuration is the file's content, as applied.
	Configuration string `json:"configuration"`
	// Changes are what differs from the configuration applied before.
	Changes []AppliedChange `json:"changes"`
}

// AppliedChange is one change of an applied configuration.
type AppliedChange struct {
	Setting string `json:"setting"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
	// Agents are the running agents that pick the change up at their next start.
	Agents []string `json:"agents,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// ConfigurationApplied journals that the user applied c at at.
func (s *Store) ConfigurationApplied(c AppliedConfiguration, at time.Time) error {
	if c.Changes == nil {
		c.Changes = []AppliedChange{}
	}
	return s.change(at, func(tx *sql.Tx) (string, any, error) {
		return KindConfigurationApplied, c, nil
	})
}
