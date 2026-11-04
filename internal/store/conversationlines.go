// SPDX-License-Identifier: Apache-2.0

package store

import "fmt"

// What reaches the user in the conversation takes two shapes (04 rules 7, 8
// and 9): an automated item is one line that starts with its job's tag, and
// anything that needs the user is a flagged block, shown once, whose first
// line starts with the flag and the job's tag. The daemon's messages to
// Coordination's leader ask for them in these words, and its instructions
// give the same shapes; the status line keeps a pending decision visible
// after its block, so nothing is shown twice.

// Flag starts the first line of a flagged block.
const Flag = "⚑"

// JobTag is the tag that starts every line about job: `job 14:`.
func JobTag(job int64) string {
	return fmt.Sprintf("job %d:", job)
}

// OneLine asks Coordination's leader to give the user an automated item about
// job as one line that starts with the job's tag.
func OneLine(job int64) string {
	return fmt.Sprintf("Tell the user in one line that starts `%s`", JobTag(job))
}

// FlaggedBlock asks Coordination's leader to show the user, once, what about
// job needs them, in a flagged block whose first line starts with the flag
// and the job's tag.
func FlaggedBlock(job int64) string {
	return fmt.Sprintf("Show the user once, in a flagged block whose first line starts `%s %s`", Flag, JobTag(job))
}

// Presentation tells Coordination's leader how to bring version r of a
// decision request to the user: once, in a flagged block, with its answer
// surface. The status line keeps it visible until it is answered, so it is
// never shown again, except after a refused record, and never a reminder.
func (r DecisionRequest) Presentation() string {
	stays := "The status line keeps it visible until they answer it, so never show it again or remind them of it"
	if r.Surface == SurfaceLavish {
		return fmt.Sprintf("%s, that decision request %d waits for them in Lavish, with its page's link from `asmai decisions`, and never its questions. %s.", FlaggedBlock(r.Job), r.Request, stays)
	}
	return fmt.Sprintf("%s, with the leader's own content, the question, each option, the recommendation and that they answer by their next message here; then run `asmai decision present --request %d --version %d`. %s; present it again only when the daemon refuses a record of the answer.", FlaggedBlock(r.Job), r.Request, r.Version, stays)
}
