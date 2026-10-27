// SPDX-License-Identifier: Apache-2.0

// Command asmai is AsmAI's single executable: the per-user daemon, and the
// commands that reach it over its Unix socket.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/config"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/logfile"
	"github.com/talvor/asmai/internal/providers"
	"github.com/talvor/asmai/internal/roles"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
	"github.com/talvor/asmai/internal/toonjson"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
// Development builds keep "dev".
var version = "dev"

const usage = `usage: asmai [<command>] [--json | --toon]

commands you type:
  asmai, asmai chat           talk with Coordination: attach this terminal to Coordination's leader,
                              starting the factory first if it is stopped. What you type reaches
                              Coordination, and each message you submit is journaled word for word.
                              Ctrl-] leaves the conversation, which pauses nothing
  asmai init [--yes] [--claude-default-model MODEL]
                              the first run: check this platform is certified, choose Claude's
                              default model and write a file staffing every role if there is none;
                              make the state directory's folders, install the pinned Claude Code
                              once you confirm (--yes confirms and agrees to open Claude Code),
                              check it and that it applies
                              AsmAI's permission rules, and open it in the agents and workspaces
                              folders and each registered repository's clone for you to trust them.
                              A step already done is skipped. asmai setup is its old name
  asmai doctor                run the same checks as init and start without changing anything, and
                              report what skill hiding cannot cover, the added and repository skills
                              (unqualified), and whether upstream has a newer skill release
  asmai start [--foreground]  start the factory: run its checks, its daemon and Coordination's leader;
                              the daemon runs in the background unless --foreground
  asmai stop                  persist the factory's state and stop its daemon and agents; queued
                              messages and open handoffs survive it, and the next start continues
                              the work: it restores Coordination and each leader with open work, and
                              runs each worker that had stopped at a boundary again, resuming its
                              native session where it can, in a new dispatch
  asmai status                show the daemon, its version and each leader
  asmai agents                list the factory's agents
  asmai jobs                  list numbered jobs
  asmai decisions             list the decisions waiting for you, each with its answer surface, the conversation
                              or Lavish, and the Lavish page's link. It only lists: it answers nothing
  asmai grants                list the grants you gave, each with its scope, what it approves, what it has
                              spent and whether it is active, revoked or invalidated. It only lists
  asmai grant revoke <id>     revoke a grant: every later action it would cover is refused, and what it
                              already authorized is reported as it stands, never undone
  asmai job <number>          show a job and its witnessed request
  asmai job clean <number>    remove the scratch workspaces of a job with no repository, and the results in them;
                              they are kept until you run this, and it is refused while the job is open
  asmai attach <agent>        show an agent's terminal and observe it; Ctrl-] detaches.
                              Address an agent as name@role, or by its role for its leader
  asmai dashboard             show the factory live in this terminal: agents, jobs and the log in three
                              tabs, 1, 2 and 3, under a banner of what waits on you; q leaves. It only
                              reads, never acts on the factory, and shows a message while the factory
                              is stopped. It needs a terminal of at least 80 columns by 24 rows
  asmai log [--follow]        print the daemon's log; --follow waits for more
  asmai export                write the journal out for inspection
  asmai providers install     fetch the pinned provider CLIs, Claude Code from its official channel and
                              AsmAI's own Lavish build from its release, after you confirm; never your
                              own copies, and Lavish needs no Node
  asmai providers list        show the provider CLIs AsmAI has installed, each marked pinned or not
  asmai lavish                list the review links waiting for you in the factory's Lavish server and the
                              SSH command that forwards its port, as it listens on this host only
  asmai repo add <path-or-url> [--name <name>]
                              register a repository: record its location, origin and default
                              branch in the store and the configuration file, and make AsmAI's
                              own clone of it, fetched from origin, then open the pinned Claude
                              Code in the clone for you to trust it, as setup does. Your own
                              checkout is never used for work. A repository with no
                              docs/agents/issue-tracker.md gets a setup job at once, whose questions reach you in
                              Lavish; with that file present nothing more happens. It also takes the entry's other
                              settings: --default-branch <branch>, --notes <text>,
                              --checks-one-at-a-time, --no-ci, --commit-trailers=false and
                              --repository-skills <role> (repeat for each role) and
                              --writable <path>[=<ENV>] (repeat for each folder), the folders
                              outside their workspaces that the repository's workers may write,
                              such as a build cache; with none, the folders its language suggests
  asmai repo list             list the registered repositories
  asmai repo show <name>      show what is recorded for a registered repository
  asmai repo remove <name>    remove a repository's record, its configuration entry and its
                              clone; refused while it has an open job
  asmai config check          read the configuration file and report every problem in it, each with
                              its line and the fix; exits 1 if there is any. Only you change the file
  asmai config show           print the effective configuration, with every default written out
  asmai config edit           open the configuration file in $VISUAL or $EDITOR and accept what you save
                              only when it has no problem; a file with problems is shown them and
                              edited again, or left as it was
  asmai config apply          hand the configuration file to the running factory: validate it, show what
                              it changes and which agents pick each change up at their next start, added
                              skills included; ask to confirm, then journal it.
                              Repository records change through repo add and repo remove
  asmai role list             list each role's provider and model for its leader and its workers, and its
                              skill lists
  asmai role set <role> [--leader-provider <p>] [--leader-model <m>]
                          [--worker-provider <p>] [--worker-model <m>]
                              set a role's provider and model in the configuration file; claude is the
                              only provider, and codex is refused
  asmai config example        print an example configuration file, with every default written out,
                              which passes asmai config check
  asmai version               print the version of this executable
  asmai notices               print AsmAI's license and the third-party notices of what is compiled into it

commands agents run:
  asmai job open --message latest|<id> [--repository <name>] --reading <text> --mandate tested-pr|planning-only|research-only --criterion <text>
                              Coordination's leader opens a job; repeat --criterion for each acceptance criterion.
                              A planning-only or research-only job is authorized only to produce its outcome, and
                              is never handed to Engineering; a research-only job may target no repository, and
                              its assignments then work in scratch workspaces
  asmai job link --job <number> --follows <number>
                              Coordination's leader links a follow-up job to the job it follows; where the
                              earlier job's pull request is still open the follow-up continues its job branch
                              and pull request, and otherwise starts afresh
  asmai job cancel <number> --message latest|<id>
                              Coordination's leader cancels a job at the user's word, whose witnessed message
                              is the reason: the daemon cancels every assignment, each ending once its branch
                              push is confirmed, closes the open pull request with a comment, withdraws the
                              pending decision requests and discards unreviewed results; branches stay on origin
  asmai brief <number>        read a job's canonical brief
  asmai inbox [--dispatch <id>]  fetch messages; a fetch records delivery once per message
  asmai handoff send --job <number> --to <leader> --outcome <text> --decisions <text> --evidence <text> --constraints <text> --permissions <text> --criterion <text> [--record <id>] [--assignment <id>]
                              repeat --record for each of the job's decision records that goes with the handoff;
                              any leader asks Quality for an earlier review of an assignment of the job with
                              --assignment, which Quality reviews at the commit of its result
  asmai handoff accept|clarify|decline --handoff <id> [--answer <text>]
  asmai assign --job <number> --outcome <text> --criterion <text> [--skill <name>] [--narrow <name>] [--record <id>] [--approved <id>]
                              Engineering's, Planning's or Research's leader, or Coordination's for the setup of a
                              repository, gives a writing assignment to a worker of its role, in a workspace of AsmAI's clone on its own branch, or for a job with no
                              repository in an empty scratch workspace; repeat --criterion for
                              each acceptance criterion. A skill that starts only on request starts only when
                              the job's mandate calls for it, or with --approved, the decision record of the
                              user's own answer to a proposal of it, which goes to the worker too
  asmai assign --read-only --commit <sha> --job <number> --outcome <text> --criterion <text> [--of <assignment>]
                              Quality's leader gives a validation to a worker, in a clean workspace fixed at the
                              job branch's exact head, with the job's mandate and criteria; the daemon decides
                              whether it reviews the changes or only re-runs the checks, from what changed
                              since the last validated commit. With --of it is the early review of an assignment
                              that a leader asked for, fixed at the commit of its result
  asmai effect <kind> <ref>   a worker or the delivery owner records an effect it made, tagged with its dispatch;
                              the delivery owner's "asmai effect pull-request <url>" also records the pull
                              request as gh shows it, its number and whether it is a draft or "[not ready]"
  asmai result --evidence <text> --test <text>|none --check '<command> -> <outcome>'|none --gap <text>|none --pr-section <text> [--artifact <text>] [--record <id>] [--grant <id>]
                              a worker submits its assignment's result; repeat a flag for each value. A result
                              names the record each human step of its skills relied on: --record for the user's
                              decision record, --grant for a grant, as the answer to the step gave them; the
                              daemon refuses a result that leaves one out
  asmai result --evidence <text> --check '<command> -> <outcome>'|none --gap <text>|none --finding '<blocking|advisory|needs-you>: <text>'|none [--resolved <id>] [--unresolved <id>]
                              a Quality worker submits its validation report
  asmai blocked --reason <text> [--needs <text>]
                              a worker reports that it cannot go on
  asmai blocked --job <number> --reason <text> --needs <text> [--blocks <assignment>]
                              a leader other than Coordination's that cannot go on, for a reason that is not the
                              user's choice, such as a missing build cache, a broken tool or an infrastructure
                              fault, files a blocked report to Coordination's leader, which is started or nudged.
                              It is not a decision request: it has no version and no answer surface, and never
                              waits on the user. Repeat --blocks for each assignment of yours that depends on it;
                              it only names them in the report and the brief, and holds nothing
  asmai blocked resolve <id> --note <text>
                              Coordination's leader resolves a leader's blocked report once its cause is settled:
                              the note, what changed, goes to the leader that filed it, which is started or nudged
  asmai accept --assignment <id> --reason <text> [--ends-job] [--prototype]
                              a leader accepts the result of an assignment it owns, and the daemon
                              fast-forwards the job branch to it; Quality's leader accepts a validation
                              report, and the daemon delivers it to the job's delivery owner. Repeat --reason
                              for each reason. For a job with no repository nothing moves, and --ends-job is
                              the owning leader accepting the job's outcome against its mandate: the job ends
                              and Coordination is told where the results are. --prototype accepts a result
                              as a prototype that stays on its own assignment branch on origin: the job
                              branch does not move, and the verdict is a note on it that points to that branch
  asmai reject --assignment <id> --reason <text>
                              a leader returns the result to the same worker, in a new dispatch
  asmai cancel --assignment <id> --reason <text>
                              a leader cancels an assignment it owns; its worker pushes its assignment
                              branch, if it has one, and stops
  asmai redispatch --assignment <id> --reason <text>
                              a leader sends an active assignment it owns, or one that needs reconciliation,
                              to its worker again, in a new dispatch
  asmai push <number>         the delivery owner, Engineering's leader for a code job and Coordination's for a
                              repository's setup, asks the daemon to
                              push the job branch: it fetches the branch from origin, refuses and records an
                              outside change if origin has commits AsmAI did not make, and otherwise pushes
                              it with a plain push, never forced, recorded as the daemon's effect
  asmai decision record --job <number> --choice <text> --relied-on <text> --rationale <text> --evidence <text> --assumption <text>|none --reopen-when <text>|none [--source delegated|human --cites <witnessed message or Lavish answer>]
                              a leader records a decision that affects later work, with the fields of rule 25:
                              itself as the accountable leader, the mandate or grant it relied on, the choice,
                              its rationale and evidence, its material assumptions and the conditions that
                              would reopen it, marked delegated (the default) or human, which cites the
                              witnessed message it came from (its journal ID, or latest) or the Lavish answer
                              the user gave after the version was shown, whose entry the owning leader's
                              message names. Repeat --assumption and --reopen-when for each.
                              A routine edit needs no record. Pass the records that matter on with
                              --record <id> on asmai assign and asmai handoff send
  asmai decision record --job <number> --choice rerun-failed --rationale <text> --evidence <text>
                              Engineering's leader records its judgment that the red checks on the pushed head
                              failed for a reason unrelated to the change, as a delegated decision, and the
                              daemon re-runs their failed jobs once; a second re-run of the same head is refused
  asmai decision record --request <id> --version <n> --cites <witnessed message> --reading <text> --choice <text> [--job <number>]
                              Coordination's leader records the user's answer to a decision request that is
                              answered in the conversation: a human decision citing the user's witnessed message
                              given after that version was shown, and refused if it cites a nudge or a message
                              typed against an older version, when the current version is presented again. It
                              stores your --reading of their words beside them, and the owning leader that asked
                              receives both; --relied-on, --rationale and --evidence default to the answer itself
  asmai decision record --grant --scope job|repository|factory [--job <number>|--repository <name>] --action spend|deploy|destructive|decision --approved <text> --condition <text>|none [--amount <n> --currency <code>] (--cites <witnessed message>|--answer <decision record>)
                              a leader records a grant the user gave in words, at its scope, citing them:
                              Coordination's leader one given in the conversation, with --cites, and the
                              leader that asked one given in an answer, with --answer, the record of that
                              answer. A grant without a valid citation is refused; spending needs the amount
                              and its currency, and no grant can approve a merge. It is carried through
                              assignments, handoffs and restarts while its scope and conditions hold, and a
                              job's own approval stays with that job unless it was granted at a wider scope
  asmai authorize --action spend|deploy|destructive --what <text> [--job <number>] [--amount <n> --currency <code>]
                              an agent asks before each consequential action whether a grant covers it, and
                              takes the action only if it does. Spending is zero unless a grant names the
                              amount and its scope; no grant covers a merge. A worker's job is its assignment's
  asmai grant invalidate <id> --because <text>
                              a leader reports that a condition of a grant no longer holds, so it needs a new
                              decision of the user's
  asmai decision present --request <id> --version <n>
                              Coordination's leader records that it showed the user that version in the
                              conversation, once; the user's answer is the message they send after it
  asmai decision request [--job <number>] [--request <id>] [--kind <kind>] --title <text> --option <text> [--recommend <option>]
  asmai decision request [--job <number>] [--request <id>] [--kind <kind>] --question-file <file> [--content <text>] [--blocks <assignment>] [--carries <request>]
                              request a decision. A worker's request goes to its owning leader, never to the user,
                              and holds its assignment's work until the answer; a leader's goes to the user through
                              Coordination, and holds the assignments of its own that --blocks names. --request
                              issues a new version, which replaces the pending one and reissues its Lavish
                              page; a single choice is answered in the conversation and everything else in
                              Lavish, where the daemon renders the questions and --content above them and
                              brings back what the user sends, word for word. --carries has a leader's request
                              carry its worker's request to the user: a worker's grilling or Wayfinder question
                              is answered only by the user's own answer to it
  asmai decision request --kind finding|dispute --finding <id> [--title <text> --option <text> ...] [--content <text>]
                              a leader takes a decision the user always makes to them, word for word: a needs-you
                              finding, which the daemon quotes from its record, or a blocking finding the two
                              leaders could not settle, whose options are to uphold it or overrule it. Overruling
                              is the user's decision and clears the finding; nothing else but Quality's
                              re-validation does
  asmai decision request --kind outside-mandate|conflict --title <text> --option <text> --content <text>
                              asks for an action outside the mandate, or for a ruling on a repository instruction
                              that conflicts with it: the work waits for the user's own answer, which no record
                              or recommendation stands in for, and the instruction is never followed against
                              the mandate
  asmai decision request --kind step --skill <skill> --step <text> --title <text>
                              a worker asks its owning leader for the human step of a skill, such as the test
                              seams of tdd: a recorded decision of the user's, or a grant, whose scope covers
                              the job satisfies it, and where none does the leader carries it to the user
  asmai decision answer --request <id> --version <n> --answer <text> [--relied-on <text>]
  asmai decision answer --request <id> --version <n> --user-answer <your request>
                              a worker's owning leader answers one version of its request from the records; an
                              answer naming an older version is refused. A worker's grilling or Wayfinder
                              question is never answered in an agent's words: --user-answer returns the
                              user's own answer to the leader's request that carried it, as they gave it in
                              Lavish, once every question of it is answered
  asmai decision answer --request <id> --version <n> --answer <text> --record <id> [--grant <id>]
                              answer a worker's step from the records: --record is the user's own decision
                              record of the job that names the skill, --grant a grant of a decision, active, whose
                              scope covers the job and that names the skill; the daemon refuses a decision of
                              another job, a delegated decision and a grant of a narrower scope
  asmai hook                  report a hook event to the daemon; agent sessions' hooks run it

Every command accepts --json to print JSON instead of tables, or --toon to print the same data as
TOON (Token-Oriented Object Notation), which takes fewer tokens than JSON; scripts and tests
use --json. The two cannot be combined. A failure prints {"error": ...} in the format asked
for, and exits 1. asmai attach --json or --toon prints the agent's screen as it is now, and asmai
chat --json or --toon Coordination's. asmai log --json prints each record as a JSON line, and
asmai log --toon each as its own TOON document, separated by a blank line.
`

// groups are the commands that are grouped by noun, with their subcommands.
var groups = map[string][]string{
	"providers": {"install", "list"},
	"repo":      {"add", "list", "show", "remove"},
	"handoff":   {"send", "accept", "clarify", "decline"},
	"decision":  {"record", "request", "answer", "present"},
	"grant":     {"revoke", "invalidate"},
	"config":    {"check", "show", "edit", "apply", "example"},
	"role":      {"list", "set"},
}

// positional are the commands that take arguments, with what to say when
// each is missing.
var positional = map[string][]string{
	"attach":           {"name the agent, such as leader@coordination or coordination"},
	"repo add":         {"name a local checkout or a URL"},
	"repo show":        {"name the repository, as `asmai repo list` shows it"},
	"repo remove":      {"name the repository, as `asmai repo list` shows it"},
	"role set":         {"name the role, one of " + strings.Join(roles.All, ", ")},
	"job show":         {"name the job number, as `asmai jobs` shows it"},
	"job clean":        {"name the job number, as `asmai jobs` shows it"},
	"job cancel":       {"name the job number, as `asmai jobs` shows it"},
	"brief":            {"name the job number, as `asmai jobs` shows it"},
	"push":             {"name the job number, as `asmai jobs` shows it"},
	"grant revoke":     {"name the grant, as `asmai grants` lists them"},
	"grant invalidate": {"name the grant, as `asmai grants` lists them"},
	"blocked resolve":  {"name the blocked report, as `asmai brief <number>` lists it"},
	"effect":           {"name the kind of effect, one word such as commit or push", "name what the effect is of, such as the commit or the branch pushed"},
}

// logPoll is how often `asmai log --follow` looks for new lines.
const logPoll = 200 * time.Millisecond

// pins is the pins file asmai installs providers from: the one embedded in
// it, except in tests.
var pins = asmai.Pins

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	original := slices.Clone(args)
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		if os.Getenv(daemon.SessionCredential) != "" {
			fmt.Fprintln(stderr, "asmai: an agent may run only its coordination commands")
			return 1
		}
		fmt.Fprint(stdout, usage)
		return 0
	}
	// A bare asmai, flags and all, is the conversation.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"chat"}, args...)
	}
	name, args := args[0], args[1:]
	if name == "setup" {
		name = "init"
	}
	if name == "job" {
		switch {
		case len(args) > 0 && args[0] == "open":
			name, args = "job open", args[1:]
		case len(args) > 0 && args[0] == "link":
			name, args = "job link", args[1:]
		case len(args) > 0 && args[0] == "clean":
			name, args = "job clean", args[1:]
		case len(args) > 0 && args[0] == "cancel":
			name, args = "job cancel", args[1:]
		default:
			name = "job show"
		}
	}
	// A worker's or a leader's blocked report is asmai blocked; Coordination
	// resolves a leader's with asmai blocked resolve.
	if name == "blocked" && len(args) > 0 && args[0] == "resolve" {
		name, args = "blocked resolve", args[1:]
	}
	if subcommands, ok := groups[name]; ok {
		if len(args) == 0 || !slices.Contains(subcommands, args[0]) {
			want := subcommands[len(subcommands)-1]
			if len(subcommands) > 1 {
				want = strings.Join(subcommands[:len(subcommands)-1], ", ") + " or " + want
			}
			fmt.Fprintf(stderr, "asmai %s: want %s\n\n%s", name, want, usage)
			return 2
		}
		name, args = name+" "+args[0], args[1:]
	}
	if name == "git-hook" {
		return gitHook(args, stdin, stdout, stderr)
	}
	flags := flag.NewFlagSet("asmai "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "print JSON instead of tables")
	toonOutput := flags.Bool("toon", false, "print TOON instead of tables")
	foreground, follow, repoName, assumeYes := new(bool), new(bool), new(string), new(bool)
	var message string
	var reading, mandate, repository string
	var handoffJob, handoffID, dispatchID, linkJob, linkFollows, handoffAssignment, assignOf int64
	var recipient, outcome, decisions, evidence, constraints, permissions, answer string
	var criteria criteriaFlags
	var assignJob int64
	var assignReadOnly bool
	var assignCommit, assignSkill string
	var assignApproved int64
	var assignNarrow listFlags
	var assignOutcome, blockedReason, blockedNeeds, blockedNote string
	var blockedJob int64
	var blockedBlocks listFlags
	var decidedAssignment int64
	var endsJob, prototype bool
	var reasons listFlags
	var repoOptions repoSettings
	var role roleChoices
	var claudeDefaultModel string
	var request requestFlags
	var decision decisionFlags
	var records listFlags
	var authorizing authorizeFlags
	var invalidation string
	var resultArgs resultFlags
	switch name {
	case "init":
		assumeYes = flags.Bool("yes", false, "confirm the install and agree to open Claude Code without asking")
		flags.StringVar(&claudeDefaultModel, "claude-default-model", "", "Claude's fallback model in a new config (default opus)")
	case "start":
		foreground = flags.Bool("foreground", false, "keep the daemon attached to this terminal")
	case "log":
		follow = flags.Bool("follow", false, "after the log, print each new line as it is written")
	case "repo add":
		repoName = flags.String("name", "", "register the repository under this name, not one made from its origin")
		flags.StringVar(&repoOptions.defaultBranch, "default-branch", "", "the branch jobs start from, not the one origin's HEAD names")
		flags.StringVar(&repoOptions.notes, "notes", "", "your notes, which count as repository instructions")
		flags.BoolVar(&repoOptions.checksOneAtATime, "checks-one-at-a-time", false, "the repository's checks cannot run side by side")
		flags.BoolVar(&repoOptions.noCI, "no-ci", false, "the repository has no CI; Quality's checks stand in")
		repoOptions.commitTrailers = true
		flags.BoolVar(&repoOptions.commitTrailers, "commit-trailers", true, "commits carry the AsmAI trailers; --commit-trailers=false switches them off")
		flags.Var(&repoOptions.skills, "repository-skills", "record the repository's own skills for this role; may be repeated")
		flags.Var(&repoOptions.writable, "writable", "a folder outside the workspace that workers of the repository's jobs may write, as <path> or <path>=<ENV>; may be repeated")
	case "role set":
		flags.StringVar(&role.leaderProvider, "leader-provider", "", "the provider of the role's leader")
		flags.StringVar(&role.leaderModel, "leader-model", "", "the model of the role's leader")
		flags.StringVar(&role.workerProvider, "worker-provider", "", "the provider of the role's workers")
		flags.StringVar(&role.workerModel, "worker-model", "", "the model of the role's workers")
	case "job open":
		flags.StringVar(&message, "message", "", "latest or the witnessed message's journal ID")
		flags.StringVar(&repository, "repository", "", "registered repository name")
		flags.StringVar(&reading, "reading", "", "Coordination's reading of the user's words")
		flags.StringVar(&mandate, "mandate", store.MandateTestedPR, "job mandate: "+strings.Join(store.Mandates[:len(store.Mandates)-1], ", ")+" or "+store.Mandates[len(store.Mandates)-1])
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
	case "job link":
		flags.Int64Var(&linkJob, "job", 0, "the follow-up job's number")
		flags.Int64Var(&linkFollows, "follows", 0, "the number of the job it follows")
	case "job cancel":
		flags.StringVar(&message, "message", "", "latest or the journal ID of the witnessed message whose words are the reason")
	case "handoff send":
		flags.Int64Var(&handoffJob, "job", 0, "job number")
		flags.StringVar(&recipient, "to", "", "receiving leader")
		flags.StringVar(&outcome, "outcome", "", "requested outcome")
		flags.StringVar(&decisions, "decisions", "", "relevant decisions")
		flags.StringVar(&evidence, "evidence", "", "relevant evidence")
		flags.StringVar(&constraints, "constraints", "", "constraints")
		flags.StringVar(&permissions, "permissions", "", "permissions granted")
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
		flags.Var(&records, "record", "the ID of one of the job's decision records that goes with the handoff; may be repeated")
		flags.Int64Var(&handoffAssignment, "assignment", 0, "with --to quality, the ID of an assignment of the job to ask for an earlier review of, at the commit of its result")
	case "handoff accept", "handoff clarify", "handoff decline":
		flags.Int64Var(&handoffID, "handoff", 0, "handoff ID")
		flags.StringVar(&answer, "answer", "", "answer, question or reason")
	case "inbox":
		flags.Int64Var(&dispatchID, "dispatch", 0, "fetch one dispatch")
	case "assign":
		flags.Int64Var(&assignJob, "job", 0, "job number")
		flags.StringVar(&assignOutcome, "outcome", "", "the outcome the worker is to deliver")
		flags.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
		flags.BoolVar(&assignReadOnly, "read-only", false, "give a validation, which changes nothing, instead of a writing assignment")
		flags.StringVar(&assignCommit, "commit", "", "with --read-only, the job branch's exact head commit the validation is fixed at, or with --of the commit of the assignment's result")
		flags.Int64Var(&assignOf, "of", 0, "with --read-only, the ID of the assignment an early review that a leader asked Quality for is of")
		flags.StringVar(&assignSkill, "skill", "", "the skill from the worker's list that it is to run")
		flags.Int64Var(&assignApproved, "approved", 0, "with --skill, a skill that starts only on request which the job's mandate does not call for: the ID of the decision record of the user's own answer that starts it")
		flags.Var(&assignNarrow, "narrow", "a skill the worker's list is narrowed to, from the role's workers' list; may be repeated")
		flags.Var(&records, "record", "the ID of one of the job's decision records that the worker is given; may be repeated")
	case "effect":
	case "result":
		flags.Var(&resultArgs.evidence, "evidence", "what shows the work does; may be repeated")
		flags.Var(&resultArgs.artifacts, "artifact", "a link to something the work produced; may be repeated")
		flags.Var(&resultArgs.tests, "test", "a test added, or none; may be repeated")
		flags.Var(&resultArgs.checks, "check", "a check run and its outcome at the commit, '<command> -> <outcome>', or none; may be repeated")
		flags.Var(&resultArgs.gaps, "gap", "an unresolved gap, or none; may be repeated")
		flags.StringVar(&resultArgs.prSection, "pr-section", "", "the worker's part of the pull request: what changed, with before-and-after evidence")
		flags.Var(&resultArgs.findings, "finding", "a validation's finding, '<blocking|advisory|needs-you>: <text>', or none; may be repeated")
		flags.Var(&resultArgs.resolved, "resolved", "an earlier blocking finding's ID that is resolved at this commit; may be repeated")
		flags.Var(&resultArgs.skills, "skill", "a skill from your list that you ran, or none; may be repeated")
		flags.Var(&resultArgs.unresolved, "unresolved", "an earlier blocking finding's ID that is still open at this commit; may be repeated")
		flags.Var(&resultArgs.records, "record", "the ID of a decision record of the user's that a human step of a skill you ran relied on; may be repeated")
		flags.Var(&resultArgs.grants, "grant", "the ID of a grant that a human step of a skill you ran relied on; may be repeated")
	case "blocked":
		flags.StringVar(&blockedReason, "reason", "", "why the worker, or with --job the leader, cannot go on")
		flags.StringVar(&blockedNeeds, "needs", "", "what would unblock the worker, or with --job the leader")
		flags.Int64Var(&blockedJob, "job", 0, "a leader's blocked report to Coordination: the job it cannot go on with")
		flags.Var(&blockedBlocks, "blocks", "with --job, an assignment you own that depends on the report; nothing is held. May be repeated")
	case "blocked resolve":
		flags.StringVar(&blockedNote, "note", "", "what changed, which goes to the leader that filed the report")
	case "accept", "reject", "cancel", "redispatch":
		flags.Int64Var(&decidedAssignment, "assignment", 0, "assignment ID")
		flags.Var(&reasons, "reason", "a reason for the decision; may be repeated")
		flags.BoolVar(&endsJob, "ends-job", false, "with accept, the owning leader accepts the outcome of a job with no repository against its mandate, which ends the job")
		flags.BoolVar(&prototype, "prototype", false, "with accept, the owning leader accepts a result as a prototype that stays on its own assignment branch: the job branch does not move")
	case "decision record":
		flags.Int64Var(&decision.job, "job", 0, "job number")
		flags.StringVar(&decision.choice, "choice", "", "what the decision chose")
		flags.StringVar(&decision.reliedOn, "relied-on", "", "the mandate or grant the decision relied on")
		flags.StringVar(&decision.rationale, "rationale", "", "why")
		flags.StringVar(&decision.evidence, "evidence", "", "what the decision rests on")
		flags.Var(&decision.assumptions, "assumption", "a material assumption the choice rests on, or none; may be repeated")
		flags.Var(&decision.reopenWhen, "reopen-when", "a condition that would reopen the decision, or none; may be repeated")
		flags.StringVar(&decision.source, "source", "", "delegated (the default), or human")
		flags.StringVar(&decision.cites, "cites", "", "with --source human, the witnessed message the decision came from: its journal ID, or latest for the newest the user sent you; or the journal ID of the Lavish answer the user gave")
		flags.Int64Var(&decision.request, "request", 0, "the decision request the user's answer in the conversation is to; Coordination's leader only")
		flags.IntVar(&decision.version, "version", 0, "with --request, the version the user's message answers")
		flags.StringVar(&decision.reading, "reading", "", "with --request, your reading of the user's words, stored beside them")
		flags.BoolVar(&decision.grant.grant, "grant", false, "record a grant the user gave in words, at its scope, instead of a decision")
		flags.StringVar(&decision.grant.scope, "scope", "", "with --grant, the scope: job (with --job), repository (with --repository) or factory")
		flags.StringVar(&decision.grant.repository, "repository", "", "with --grant --scope repository, the registered repository")
		flags.StringVar(&decision.grant.action, "action", "", "with --grant, the action approved: spend, deploy, destructive or decision; no grant approves a merge")
		flags.StringVar(&decision.grant.approved, "approved", "", "with --grant, the exact decision or action the user approved")
		flags.StringVar(&decision.grant.amount, "amount", "", "with --grant --action spend, the most it lets be spent, such as 10 or 12.50")
		flags.StringVar(&decision.grant.currency, "currency", "", "with --grant --action spend, its currency, such as USD")
		flags.Var(&decision.grant.conditions, "condition", "with --grant, a condition the grant holds under, or none; may be repeated")
		flags.Int64Var(&decision.grant.answer, "answer", 0, "with --grant, the decision record of the user's answer that held the grant, which the leader that asked cites instead of --cites")
	case "decision request":
		flags.Int64Var(&request.job, "job", 0, "job number; a leader's request names its job, and a worker's is about its assignment's")
		flags.Int64Var(&request.request, "request", 0, "the request this is a new version of, which it replaces")
		flags.StringVar(&request.kind, "kind", "", "choice or provider (one question with options), finding (a needs-you finding, quoted word for word), dispute (a blocking finding the leaders could not settle), outside-mandate (an action the mandate does not cover), conflict (a repository instruction that conflicts with the mandate), review, grilling, wayfinder, questions or step (a worker's request for the human step of a skill); the default is choice for one question with options, questions otherwise")
		flags.Var(&request.findings, "finding", "with --kind finding or dispute, the ID of a finding of the job the request is about; may be repeated. The daemon quotes it to the user from its record")
		flags.StringVar(&request.skill, "skill", "", "with --kind step, the skill whose human step you ask for, such as tdd")
		flags.StringVar(&request.step, "step", "", "with --kind step, the human step you ask for, such as the test seams")
		flags.StringVar(&request.content, "content", "", "your own content above the questions, such as a prototype, a diff or a report")
		flags.StringVar(&request.title, "title", "", "the title of the one question")
		flags.StringVar(&request.body, "body", "", "the body of the one question")
		flags.Var(&request.options, "option", "an option of the one question; may be repeated")
		flags.StringVar(&request.recommend, "recommend", "", "the option you recommend for the one question")
		flags.Var(&request.questions, "question", `a question as JSON, {"id": ..., "title": ..., "body": ..., "options": [...], "recommendation": ...}; may be repeated`)
		flags.Var(&request.questionFiles, "question-file", "a file holding one question as JSON, as --question does: an agent's command line must keep a brace away from a quote character, so it writes the JSON to a file; may be repeated")
		flags.Var(&request.blocks, "blocks", "an assignment you own whose work waits for the answer; may be repeated")
		flags.Int64Var(&request.carries, "carries", 0, "the worker's decision request, which you own, that your request carries to the user; its answer is the user's own, returned with decision answer --user-answer")
	case "authorize":
		flags.Int64Var(&authorizing.job, "job", 0, "a leader's job; a worker's is its assignment's")
		flags.StringVar(&authorizing.action, "action", "", "the consequential action: spend, deploy or destructive")
		flags.StringVar(&authorizing.what, "what", "", "what the action is, such as the service bought or the environment deployed to")
		flags.StringVar(&authorizing.amount, "amount", "", "with --action spend, the amount, such as 4 or 12.50")
		flags.StringVar(&authorizing.currency, "currency", "", "with --action spend, its currency, such as USD")
	case "grant invalidate":
		flags.StringVar(&invalidation, "because", "", "the condition of the grant that no longer holds")
	case "grants", "grant revoke":
	case "decision present":
		flags.Int64Var(&request.request, "request", 0, "the decision request")
		flags.IntVar(&request.version, "version", 0, "the version you showed the user")
	case "decision answer":
		flags.Int64Var(&request.request, "request", 0, "the decision request")
		flags.IntVar(&request.version, "version", 0, "the version the answer is to")
		flags.StringVar(&request.answer, "answer", "", "the answer")
		flags.StringVar(&request.reliedOn, "relied-on", "", "the record the answer rests on")
		flags.Var(&request.records, "record", "the ID of the user's decision record of the job that satisfies the step the request asks for; may be repeated")
		flags.Var(&request.grants, "grant", "the ID of a grant of a decision, whose scope covers the job, that satisfies the step the request asks for; may be repeated")
		flags.Int64Var(&request.userAnswer, "user-answer", 0, "instead of --answer, your request that carried the worker's question to the user, which the user answered in Lavish: its words are the worker's answer, as the user gave them")
	case "chat", "dashboard", "doctor", "decisions", "stop", "status", "agents", "jobs", "job show", "job clean", "brief", "push", "attach", "hook", "export", "version", "notices", "providers install", "providers list", "lavish", "repo list", "repo show", "repo remove", "config check", "config example", "config show", "config edit", "config apply", "role list":
	default:
		fmt.Fprintf(stderr, "asmai: unknown command %q\n\n%s", name, usage)
		return 2
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	var operands []string
	for _, want := range positional[name] {
		if flags.NArg() == 0 {
			fmt.Fprintf(stderr, "asmai %s: %s\n\n%s", name, want, usage)
			return 2
		}
		operands = append(operands, flags.Arg(0))
		// Flags may come after an argument too.
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return 2
		}
	}
	var arg string
	if len(operands) > 0 {
		arg = operands[0]
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "asmai %s: unexpected argument %q\n\n%s", name, flags.Arg(0), usage)
		return 2
	}
	if *jsonOutput && *toonOutput {
		fmt.Fprintf(stderr, "asmai %s: --json and --toon cannot be used together: choose one output format\n", name)
		return 1
	}
	o := &output{stdout: stdout, stderr: stderr, json: *jsonOutput, toon: *toonOutput}
	if os.Getenv(daemon.SessionCredential) != "" && !slices.Contains([]string{"status", "agents", "jobs", "brief", "job open", "job link", "job cancel", "hook", "inbox", "handoff send", "handoff accept", "handoff clarify", "handoff decline", "assign", "effect", "result", "blocked", "blocked resolve", "accept", "reject", "cancel", "redispatch", "push", "decision record", "decision request", "decision answer", "decision present", "authorize", "grant invalidate"}, name) {
		if slices.Contains([]string{"init", "doctor", "start", "stop", "providers install", "providers list", "lavish", "repo add", "repo list", "repo show", "repo remove", "config check", "config example", "config show", "config edit", "config apply", "role list", "role set", "grants", "grant revoke", "job clean"}, name) {
			return o.fail(fmt.Errorf("an agent cannot run this command; ask the user to run `asmai %s`", strings.Join(original, " ")))
		}
		return o.fail(fmt.Errorf("an agent cannot run asmai %s; use an agent coordination command", name))
	}

	switch name {
	case "version":
		if o.structured() {
			return o.printData(map[string]string{"version": version})
		}
		fmt.Fprintf(stdout, "asmai %s\n", version)
		return 0
	case "notices":
		if o.structured() {
			return o.printData(map[string]string{"license": string(asmai.License), "third_party_notices": string(asmai.ThirdPartyNotices)})
		}
		stdout.Write(asmai.License)
		fmt.Fprintln(stdout)
		stdout.Write(asmai.ThirdPartyNotices)
		return 0
	case "config check":
		return configCheck(o)
	case "config example":
		return configExample(o)
	case "config show":
		return configShow(o)
	case "config edit":
		return configEdit(o, stdin)
	case "role list":
		return roleList(o)
	case "role set":
		role.present = map[string]bool{}
		flags.Visit(func(f *flag.Flag) { role.present[f.Name] = true })
		return roleSet(o, arg, role)
	}

	paths, err := statedir.Default()
	if err != nil {
		return o.fail(err)
	}
	switch name {
	case "init":
		return initFactory(o, paths, stdin, *assumeYes, claudeDefaultModel)
	case "doctor":
		return doctor(o, paths)
	case "start":
		if *foreground {
			return startForeground(o, paths)
		}
		return start(o, paths)
	case "stop":
		return stop(o, paths)
	case "status":
		return status(o, paths)
	case "agents":
		return agents(o, paths)
	case "jobs":
		return jobs(o, paths)
	case "job show":
		return jobShow(o, paths, arg)
	case "job clean":
		return jobClean(o, paths, arg)
	case "job link":
		return jobLink(o, paths, linkJob, linkFollows)
	case "job cancel":
		return jobCancel(o, paths, arg, message)
	case "job open":
		return jobOpen(o, paths, message, repository, reading, mandate, criteria)
	case "brief":
		return jobBrief(o, paths, arg)
	case "inbox":
		return inbox(o, paths, dispatchID)
	case "handoff send":
		return handoffSend(o, paths, handoffJob, recipient, outcome, decisions, evidence, constraints, permissions, criteria, records, handoffAssignment)
	case "handoff accept", "handoff clarify", "handoff decline":
		return handoffAnswer(o, paths, name, handoffID, answer)
	case "assign":
		return assign(o, paths, assignJob, assignOutcome, criteria, assignReadOnly, assignCommit, assignSkill, assignNarrow, assignApproved, records, assignOf)
	case "effect":
		return effect(o, paths, operands[0], operands[1])
	case "result":
		return result(o, paths, resultArgs)
	case "blocked":
		if blockedJob != 0 || len(blockedBlocks) > 0 {
			return leaderBlocked(o, paths, blockedJob, blockedReason, blockedNeeds, blockedBlocks)
		}
		return blocked(o, paths, blockedReason, blockedNeeds)
	case "blocked resolve":
		return blockedResolve(o, paths, arg, blockedNote)
	case "accept", "reject", "cancel", "redispatch":
		return decide(o, paths, name, decidedAssignment, reasons, endsJob, prototype)
	case "push":
		return push(o, paths, arg)
	case "decision record":
		return decisionRecord(o, paths, decision)
	case "decision request":
		return decisionRequest(o, paths, request)
	case "decision answer":
		return decisionAnswer(o, paths, request)
	case "decision present":
		return decisionPresent(o, paths, request)
	case "decisions":
		return listDecisions(o, paths)
	case "authorize":
		return authorize(o, paths, authorizing)
	case "grants":
		return listGrants(o, paths)
	case "grant revoke":
		return grantEnd(o, paths, daemon.CommandGrantRevoke, parseGrantID(arg), "")
	case "grant invalidate":
		return grantEnd(o, paths, daemon.CommandGrantInvalidate, parseGrantID(arg), invalidation)
	case "chat":
		return chat(o, paths, stdin)
	case "dashboard":
		return dashboardCommand(o, paths, stdin)
	case "attach":
		return attach(o, paths, arg, stdin)
	case "hook":
		return hook(o, paths, stdin)
	case "log":
		return printLog(o, paths, *follow)
	case "providers install":
		return providersInstall(o, paths, stdin)
	case "providers list":
		return providersList(o, paths)
	case "lavish":
		return lavishCommand(o, paths)
	case "repo add":
		return repoAdd(o, paths, arg, *repoName, repoOptions, stdin)
	case "config apply":
		return configApply(o, paths, stdin)
	case "repo list":
		return repoList(o, paths)
	case "repo show":
		return repoShow(o, paths, arg)
	case "repo remove":
		return repoRemove(o, paths, arg)
	default: // export
		return export(o, paths)
	}
}

// start starts the daemon in the background and returns once it answers.
func start(o *output, paths statedir.Paths) int {
	executable, err := os.Executable()
	if err != nil {
		return o.fail(fmt.Errorf("finding this executable: %w", err))
	}
	st, already, err := daemon.Start(paths, executable)
	if err != nil {
		return o.fail(err)
	}
	return o.startFactory(st, already, paths)
}

// startFactory has the running daemon run the checks and start Coordination's
// leader, and reports what happened.
func (o *output) startFactory(st daemon.Status, already bool, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStart})
	if err != nil {
		return o.fail(err)
	}
	return o.started(st, already, paths, resp)
}

// startForeground runs the daemon in this process, showing its log on
// stderr, until `asmai stop`, an interrupt or a termination signal.
func startForeground(o *output, paths statedir.Paths) int {
	if resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); err == nil {
		return o.startFactory(*resp.Status, true, paths)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		return o.fail(fmt.Errorf("finding this executable: %w", err))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return o.fail(fmt.Errorf("finding the configuration file: %w", err))
	}
	configFile, err := config.ActivePath(home)
	if err != nil {
		return o.fail(err)
	}
	p, err := providers.ParsePins(asmai.Pins)
	if err != nil {
		return o.fail(err)
	}
	cfg := daemon.Config{Paths: paths, Version: version, ConfigFile: configFile, Executable: executable, Pins: p, ClaudeRecord: providers.ClaudeCodeRecord(home, os.Getenv)}
	// Started in the background, the daemon's stderr is its log, which needs
	// no copy.
	if !isFile(o.stderr, paths.Log) {
		cfg.Mirror = o.stderr
		if !o.structured() {
			cfg.Mirror = &rendered{w: o.stderr}
		}
	}
	err = daemon.Run(ctx, cfg)
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		if resp, callErr := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus}); callErr == nil {
			return o.startFactory(*resp.Status, true, paths)
		}
	}
	if err != nil {
		return o.fail(err)
	}
	return 0
}

func (o *output) started(st daemon.Status, already bool, paths statedir.Paths, resp daemon.Response) int {
	var failures []daemon.Check
	for _, c := range resp.Checks {
		if !c.OK {
			failures = append(failures, c)
		}
	}
	if len(failures) > 0 {
		daemonState := "Started the factory's daemon"
		if already {
			daemonState = "The factory's daemon is running"
		}
		message := daemonState + ", but not Coordination's leader: " + plural(len(failures), "a check failed", "checks failed")
		for _, l := range resp.Leaders {
			if l.Agent == "leader@coordination" && l.State == store.AgentRunning {
				message = daemonState + " and so is Coordination's leader, but " + plural(len(failures), "a check failed", "checks failed")
			}
		}
		if o.structured() {
			o.printData(map[string]any{"error": message, "started": !already, "daemon": runningJSON(st), "checks": resp.Checks, "leaders": resp.Leaders, "state_dir": paths.Dir})
			return 1
		}
		fmt.Fprintf(o.stderr, "asmai: %s.\n", message)
		for _, c := range resp.Checks {
			if c.OK {
				fmt.Fprintf(o.stderr, "  ok      %s\n", c.Name)
				if c.Warning != "" {
					fmt.Fprintf(o.stderr, "          warning: %s\n", c.Warning)
				}
				continue
			}
			fmt.Fprintf(o.stderr, "  failed  %s: %s\n", c.Name, c.Problem)
			fmt.Fprintf(o.stderr, "          fix: %s\n", strings.ReplaceAll(c.Fix, "\n", "\n            "))
		}
		fmt.Fprintln(o.stderr, "Fix what failed, then run `asmai start` again.")
		return 1
	}
	if o.structured() {
		return o.printData(map[string]any{"started": !already, "daemon": runningJSON(st), "checks": resp.Checks, "leaders": resp.Leaders, "state_dir": paths.Dir})
	}
	if already {
		fmt.Fprintln(o.stdout, "The factory is already running; no second daemon was started.")
	} else {
		fmt.Fprintln(o.stdout, "Started the factory.")
	}
	for _, c := range resp.Checks {
		if c.Warning != "" {
			fmt.Fprintf(o.stderr, "asmai: warning, %s: %s\n", c.Name, c.Warning)
		}
	}
	o.table(append(runningRows(st, paths), leaderRows(resp.Leaders)...))
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func leaderRows(leaders []daemon.Leader) [][]string {
	var rows [][]string
	for _, l := range leaders {
		row := []string{l.Agent, l.State}
		if l.Input != "" {
			row = append(row, "input: "+l.Input)
		}
		if l.Conversation != "" {
			row = append(row, "conversation open in "+l.Conversation)
		}
		rows = append(rows, row)
	}
	return rows
}

func stop(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStop})
	if errors.Is(err, daemon.ErrNotRunning) {
		if o.structured() {
			return o.printData(map[string]any{"stopped": false, "daemon": map[string]any{"running": false}})
		}
		fmt.Fprintln(o.stdout, "The factory is not running.")
		return 0
	}
	if err != nil {
		return o.fail(err)
	}
	if o.structured() {
		return o.printData(map[string]any{"stopped": true, "daemon": map[string]any{"running": false, "version": resp.Status.Version, "pid": resp.Status.PID}})
	}
	fmt.Fprintln(o.stdout, "Stopped the factory; its state is saved in the store.")
	o.table([][]string{
		{"daemon", "stopped"},
		{"version", resp.Status.Version},
		{"pid", strconv.Itoa(resp.Status.PID)},
		{"store", paths.Store},
	})
	return 0
}

func status(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandStatus})
	if errors.Is(err, daemon.ErrNotRunning) {
		if o.structured() {
			return o.printData(map[string]any{"daemon": map[string]any{"running": false}, "state_dir": paths.Dir})
		}
		o.table([][]string{{"daemon", "not running"}, {"state directory", paths.Dir}})
		return 0
	}
	if err != nil {
		return o.fail(err)
	}
	checks := resp.Checks
	if checks == nil {
		checks = []daemon.Check{}
	}
	if o.structured() {
		prompts := resp.Prompts
		if prompts == nil {
			prompts = []daemon.WaitingPrompt{}
		}
		return o.printData(map[string]any{"daemon": runningJSON(*resp.Status), "leaders": resp.Leaders, "prompts": prompts, "checks": checks, "state_dir": paths.Dir})
	}
	rows := append(runningRows(*resp.Status, paths), leaderRows(resp.Leaders)...)
	for _, p := range resp.Prompts {
		rows = append(rows, []string{"native prompt", p.String()})
	}
	failed := false
	for _, c := range checks {
		if !c.OK {
			rows = append(rows, []string{"check failed", c.Name + ": " + c.Problem})
			failed = true
		}
	}
	o.table(rows)
	if failed {
		fmt.Fprintln(o.stdout, "`asmai start` shows how to fix what failed.")
	}
	return 0
}

// agents lists the agents the store records.
func agents(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandAgents})
	if err != nil {
		return o.fail(err)
	}
	list := resp.Agents
	if list == nil {
		list = []store.Agent{}
	}
	if o.structured() {
		return o.printData(map[string]any{"agents": list})
	}
	if len(list) == 0 {
		fmt.Fprintln(o.stdout, "No agent has run yet; `asmai start` starts Coordination's leader.")
		return 0
	}
	rows := [][]string{{"AGENT", "STATE", "GENERATION", "PROVIDER", "VERSION", "MODEL", "PID", "STARTED"}}
	for _, a := range list {
		pid := "-"
		if a.State == store.AgentRunning {
			pid = strconv.Itoa(a.PID)
		}
		rows = append(rows, []string{a.Agent, a.State, strconv.Itoa(a.Generation), a.Provider, a.Version, a.Model, pid, a.StartedAt.Local().Format(time.RFC3339)})
	}
	o.table(rows)
	return 0
}

// hook reports what an agent session's hook was given on stdin to the
// daemon, which journals it as an observation of the session named by the
// credential in the environment. It prints nothing on stdout, because Claude
// Code adds what some hooks print to the session, and it never exits 2,
// which would make Claude Code block what the hook reports.
func hook(o *output, paths statedir.Paths, stdin io.Reader) int {
	credential := os.Getenv(daemon.SessionCredential)
	if credential == "" {
		return o.fail(fmt.Errorf("hook: there is no %s: only an agent session's hooks run asmai hook", daemon.SessionCredential))
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return o.fail(fmt.Errorf("hook: reading the payload: %w", err))
	}
	if !json.Valid(payload) {
		return o.fail(errors.New("hook: the payload on stdin is not JSON"))
	}
	if _, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandHook, Session: credential, Payload: payload}); err != nil {
		return o.fail(fmt.Errorf("hook: %w", err))
	}
	if o.structured() {
		return o.printData(map[string]bool{"journaled": true})
	}
	return 0
}

func runningJSON(st daemon.Status) map[string]any {
	return map[string]any{"running": true, "version": st.Version, "pid": st.PID, "started_at": st.StartedAt}
}

func runningRows(st daemon.Status, paths statedir.Paths) [][]string {
	return [][]string{
		{"daemon", "running"},
		{"version", st.Version},
		{"pid", strconv.Itoa(st.PID)},
		{"started", st.StartedAt.Format(time.RFC3339)},
		{"state directory", paths.Dir},
	}
}

// printLog prints the daemon's log from its files, so that it can be read
// when the daemon is not running, such as after a failed start. With --json
// it prints the log's own JSON lines. With --toon it prints each record as
// its own TOON document, followed by a blank line, since a log that is
// followed has no end to hold the records in one array.
func printLog(o *output, paths statedir.Paths, follow bool) int {
	line := func(l []byte) error {
		if !o.structured() {
			_, err := fmt.Fprintln(o.stdout, logfile.Format(l))
			return err
		}
		record := l
		if !json.Valid(l) || !bytes.HasPrefix(bytes.TrimSpace(l), []byte("{")) {
			// Output the daemon wrote that is not a log record, such as a
			// crash report.
			record, _ = json.Marshal(map[string]string{"text": string(l)})
		}
		if o.toon {
			encoded, err := toonjson.FromJSON(record)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(o.stdout, "%s\n\n", encoded)
			return err
		}
		_, err := fmt.Fprintf(o.stdout, "%s\n", record)
		return err
	}
	var err error
	if follow {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = logfile.Follow(ctx, paths.Log, logPoll, line)
	} else {
		err = logfile.Read(paths.Log, line)
	}
	if err != nil {
		return o.fail(fmt.Errorf("reading the log: %w", err))
	}
	return 0
}

func export(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandExport})
	if err != nil {
		return o.fail(err)
	}
	journal := resp.Journal
	if journal == nil {
		journal = []store.Entry{}
	}
	if o.structured() {
		return o.printData(map[string]any{"journal": journal})
	}
	rows := [][]string{{"ID", "TIME", "KIND", "DETAILS"}}
	for _, e := range journal {
		rows = append(rows, []string{strconv.FormatInt(e.ID, 10), e.At.Local().Format(time.RFC3339), e.Kind, details(e.Data)})
	}
	o.table(rows)
	return 0
}

// providersInstall installs the pinned providers: Claude Code from its
// official channel, and AsmAI's own Lavish build from the release of
// talvor/asmai that publishes it. It shows what it will fetch and from where,
// and once the user confirms, fetches each, checks it against the pins file,
// keeps it in the state directory and has the daemon record it. A pin already
// installed is left as it is. It never runs or changes the user's own Claude
// Code or Lavish, and never touches ~/.claude or ~/.lavish-axi.
func providersInstall(o *output, paths statedir.Paths, stdin io.Reader) int {
	plans, notes, err := pinnedPlans(paths)
	if err != nil {
		return o.fail(err)
	}
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviders})
	if err != nil {
		return o.fail(err)
	}
	// A copy at its path that no longer matches the pin, as one that updated
	// itself does not, is fetched again.
	var todo []providers.Plan
	var installs []store.ProviderInstall
	for _, plan := range plans {
		present, err := providers.Present(plan)
		if err != nil {
			return o.fail(err)
		}
		i := slices.IndexFunc(resp.Providers, func(installed store.ProviderInstall) bool {
			return installed.Name == plan.Name && installed.Version == plan.Version && installed.Path == plan.Path && present
		})
		if i < 0 {
			todo = append(todo, plan)
		} else {
			installs = append(installs, resp.Providers[i])
		}
	}

	// With --json or --toon, stdout carries only the result.
	w := o.stdout
	if o.structured() {
		w = o.stderr
	}
	for _, note := range notes {
		fmt.Fprintln(w, note)
	}
	for _, installed := range installs {
		fmt.Fprintf(w, "%s %s is already installed; nothing to do.\n", providerTitle(installed.Name), installed.Version)
		if !o.structured() {
			o.table(installRows(installed))
		}
	}
	if len(todo) == 0 {
		return o.printInstalls(false, installs)
	}
	if err := confirmInstall(w, stdin, todo, false); err != nil {
		return o.fail(err)
	}
	done := 0
	for _, plan := range todo {
		if err := fetchOne(w, plan); err != nil {
			if done == 0 {
				return o.fail(fmt.Errorf("%w; nothing was installed", err))
			}
			return o.fail(fmt.Errorf("%w; %s was not installed", err, plan.Title))
		}
		installed, err := recordInstall(paths, plan)
		if err != nil {
			return o.fail(err)
		}
		done++
		installs = append(installs, installed)
		if !o.structured() {
			fmt.Fprintf(o.stdout, "Installed %s %s.\n", plan.Title, plan.Version)
			o.table(installRows(installed))
		}
	}
	return o.printInstalls(true, installs)
}

// printInstalls prints the result of `asmai providers install` as JSON when
// asked: whether it installed anything, Claude Code's install as provider,
// and every install as providers. It prints nothing otherwise.
func (o *output) printInstalls(installed bool, installs []store.ProviderInstall) int {
	if !o.structured() {
		return 0
	}
	result := map[string]any{"installed": installed, "providers": installs}
	for _, i := range installs {
		if i.Name == providers.ClaudeCode {
			result["provider"] = i
		}
	}
	return o.printData(result)
}

// providerTitle is a provider's name for people.
func providerTitle(name string) string {
	switch name {
	case providers.ClaudeCode:
		return "Claude Code"
	case providers.Lavish:
		return "Lavish"
	}
	return name
}

// pinnedPlan plans installing the pinned Claude Code for this platform in
// the state directory paths.
func pinnedPlan(paths statedir.Paths) (providers.Plan, error) {
	p, err := providers.ParsePins(pins)
	if err != nil {
		return providers.Plan{}, err
	}
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return providers.Plan{}, fmt.Errorf("Claude Code is not published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return p.ClaudeCodePlan(platform, paths.Providers)
}

// pinnedPlans plans installing every provider the pins file pins for this
// platform in the state directory paths: Claude Code, then the Lavish build
// when the pins file has one published for the platform. A Lavish build
// pinned but not yet published for the platform is not an error: notes says
// so.
func pinnedPlans(paths statedir.Paths) (plans []providers.Plan, notes []string, err error) {
	plan, err := pinnedPlan(paths)
	if err != nil {
		return nil, nil, err
	}
	plans = []providers.Plan{plan}
	lavish, note, err := pinnedLavishPlan(paths)
	if err != nil {
		return nil, nil, err
	}
	if note != "" {
		notes = append(notes, note)
	}
	if lavish != nil {
		plans = append(plans, *lavish)
	}
	return plans, notes, nil
}

// pinnedLavishPlan plans installing the pinned Lavish build for this
// platform. It returns no plan, and says why in note, when the pins file pins
// no Lavish, or the Lavish build is not yet published for this platform.
func pinnedLavishPlan(paths statedir.Paths) (plan *providers.Plan, note string, err error) {
	p, err := providers.ParsePins(pins)
	if err != nil {
		return nil, "", err
	}
	if p.Lavish == nil {
		return nil, "", nil
	}
	platform, ok := providers.Platform(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return nil, fmt.Sprintf("Lavish is not built for %s/%s.", runtime.GOOS, runtime.GOARCH), nil
	}
	lavish, err := p.LavishPlan(platform, paths.Providers)
	if errors.Is(err, providers.ErrNoLavishBuild) {
		return nil, fmt.Sprintf("Lavish %s is pinned, but its build is not published for %s yet, so it is not installed.", p.Lavish.Build(), platform), nil
	}
	if err != nil {
		return nil, "", err
	}
	return &lavish, "", nil
}

// ownCopy says what installing plan leaves alone.
func ownCopy(plan providers.Plan) string {
	switch plan.Name {
	case providers.Lavish:
		return "It is AsmAI's own copy: your own Lavish and ~/.lavish-axi are not touched, and it needs no Node."
	default:
		return fmt.Sprintf("It is AsmAI's own copy: your own %s and ~/.claude are not touched.", plan.Title)
	}
}

// fetchPinned shows on w what installing plan fetches, from where, how it is
// checked and where it is kept, and once the user confirms on stdin fetches
// it, unless the pinned download is already at its path.
func fetchPinned(w io.Writer, stdin io.Reader, plan providers.Plan) error {
	return fetchPinnedAs(w, stdin, plan, false)
}

// fetchPinnedAs is fetchPinned, except that assumeYes confirms the install
// without asking.
func fetchPinnedAs(w io.Writer, stdin io.Reader, plan providers.Plan, assumeYes bool) error {
	if err := confirmInstall(w, stdin, []providers.Plan{plan}, assumeYes); err != nil {
		return err
	}
	if err := fetchOne(w, plan); err != nil {
		return fmt.Errorf("%w; nothing was installed", err)
	}
	return nil
}

// confirmInstall shows on w what installing plans fetches, from where, how
// each is checked and where it is kept, and asks the user on stdin to
// confirm, unless assumeYes confirms without asking.
func confirmInstall(w io.Writer, stdin io.Reader, plans []providers.Plan, assumeYes bool) error {
	for _, plan := range plans {
		fmt.Fprintf(w, "asmai will install %s %s, as pinned in this asmai:\n", plan.Title, plan.Version)
		writeTable(w, [][]string{
			{"  fetch", plan.URL},
			{"  from", fmt.Sprintf("%s (%d MB)", plan.Source, (plan.Download.Size+1<<20-1)>>20)},
			{"  check", "SHA-256 " + plan.Download.SHA256},
			{"  keep at", plan.Path},
		})
		fmt.Fprintln(w, ownCopy(plan))
	}
	it := "it"
	if len(plans) > 1 {
		it = "them"
	}
	if assumeYes {
		fmt.Fprintf(w, "Installing %s: --yes confirms.\n", it)
		return nil
	}
	fmt.Fprintf(w, "Install %s? [y/N] ", it)
	answer := readLine(stdin)
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		if !strings.HasSuffix(answer, "\n") {
			fmt.Fprintln(w)
		}
		return errors.New("not confirmed; nothing was fetched or installed")
	}
	return nil
}

// fetchOne fetches plan's pinned download, checks it against the pins file
// and keeps it at its path, unless it is there already.
func fetchOne(w io.Writer, plan providers.Plan) error {
	present, err := providers.Present(plan)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	fmt.Fprintf(w, "Fetching %s...\n", plan.URL)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return providers.Fetch(ctx, http.DefaultClient, plan)
}

// readLine reads one line from r, newline included, a byte at a time, so
// that nothing after it is taken from what a program started next reads.
func readLine(r io.Reader) string {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			line = append(line, b[0])
			if b[0] == '\n' {
				return string(line)
			}
		}
		if err != nil {
			return string(line)
		}
	}
}

// recordInstall has the daemon record plan's install, and returns the
// install as the store holds it.
func recordInstall(paths statedir.Paths, plan providers.Plan) (store.ProviderInstall, error) {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviderInstalled, Provider: &store.ProviderInstall{
		Name:    plan.Name,
		Version: plan.Version,
		Path:    plan.Path,
		SHA256:  plan.Download.SHA256,
	}})
	if err != nil {
		return store.ProviderInstall{}, err
	}
	if len(resp.Providers) != 1 {
		return store.ProviderInstall{}, errors.New("the daemon did not answer with the install it recorded")
	}
	return resp.Providers[0], nil
}

func installRows(p store.ProviderInstall) [][]string {
	return [][]string{
		{"provider", p.Name},
		{"version", p.Version},
		{"path", p.Path},
		{"installed", p.InstalledAt.Local().Format(time.RFC3339)},
	}
}

// listedProvider is an install as `asmai providers list` shows it: what the
// store recorded, and how it stands against the pins file.
type listedProvider struct {
	store.ProviderInstall
	// State is pinned, superseded, missing or a mismatch with the pinned
	// download.
	State string `json:"state"`
}

// pendingProvider is a provider the pins file pins that is not installed.
type pendingProvider struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// providersList shows the provider installs the store records, each with how
// it stands against the pins file: only the pinned download is one AsmAI
// runs, so a copy that is not, because it is of an older pin, is missing or
// is not the pinned bytes, is marked, and so is each pinned provider that is
// not installed.
func providersList(o *output, paths statedir.Paths) int {
	resp, err := daemon.Call(paths.Socket, daemon.Request{Command: daemon.CommandProviders})
	if err != nil {
		return o.fail(err)
	}
	plans, notes, err := pinnedPlans(paths)
	if err != nil {
		return o.fail(err)
	}
	installs := []listedProvider{}
	for _, p := range resp.Providers {
		state := providers.StateSuperseded
		for _, plan := range plans {
			if plan.Name != p.Name {
				continue
			}
			if state, err = plan.Standing(p.Version); err != nil {
				return o.fail(err)
			}
		}
		installs = append(installs, listedProvider{ProviderInstall: p, State: state})
	}
	notInstalled := []pendingProvider{}
	for _, plan := range plans {
		if !slices.ContainsFunc(installs, func(i listedProvider) bool { return i.Name == plan.Name && i.Version == plan.Version }) {
			notInstalled = append(notInstalled, pendingProvider{Name: plan.Name, Version: plan.Version})
		}
	}
	if o.structured() {
		return o.printData(map[string]any{"providers": installs, "not_installed": notInstalled})
	}
	if len(installs) == 0 {
		fmt.Fprintln(o.stdout, "No providers are installed; install the pinned ones with `asmai providers install`.")
		for _, note := range notes {
			fmt.Fprintln(o.stdout, note)
		}
		return 0
	}
	rows := [][]string{{"NAME", "VERSION", "STATE", "PATH", "INSTALLED"}}
	for _, p := range installs {
		rows = append(rows, []string{p.Name, p.Version, p.State, p.Path, p.InstalledAt.Local().Format(time.RFC3339)})
	}
	o.table(rows)
	for _, p := range installs {
		switch p.State {
		case providers.StateMismatch:
			fmt.Fprintf(o.stdout, "%s %s is not the pinned download, so AsmAI does not run it; `asmai providers install` puts the pinned copy back.\n", providerTitle(p.Name), p.Version)
		case providers.StateMissing:
			fmt.Fprintf(o.stdout, "%s %s is not at %s any more; `asmai providers install` installs it again.\n", providerTitle(p.Name), p.Version, p.Path)
		case providers.StateSuperseded:
			fmt.Fprintf(o.stdout, "%s %s is not the version this asmai pins, so AsmAI does not run it.\n", providerTitle(p.Name), p.Version)
		}
	}
	for _, p := range notInstalled {
		fmt.Fprintf(o.stdout, "%s %s is pinned but not installed; install it with `asmai providers install`.\n", providerTitle(p.Name), p.Version)
	}
	for _, note := range notes {
		fmt.Fprintln(o.stdout, note)
	}
	return 0
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// details renders a journal entry's data as key=value pairs.
func details(data json.RawMessage) string {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return string(data)
	}
	var b bytes.Buffer
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		v := fields[k]
		if s, ok := v.(string); ok {
			fmt.Fprintf(&b, "%s=%s", k, s)
		} else {
			encoded, _ := json.Marshal(v)
			fmt.Fprintf(&b, "%s=%s", k, encoded)
		}
	}
	return b.String()
}

// output prints a command's result: readable tables for the user, JSON with
// --json for scripts and tests, or TOON with --toon, the same data in fewer
// tokens for agents. The two flags are never both set.
type output struct {
	stdout, stderr io.Writer
	json, toon     bool
}

// structured reports whether the result is data, in either format, and not
// tables for the user.
func (o *output) structured() bool {
	return o.json || o.toon
}

// printData prints v in the format asked for. TOON is encoded from the JSON
// v marshals to, so the two print the same data.
func (o *output) printData(v any) int {
	if o.toon {
		encoded, err := toonjson.Marshal(v)
		if err != nil {
			fmt.Fprintf(o.stderr, "asmai: %v\n", err)
			return 1
		}
		if _, err := fmt.Fprintf(o.stdout, "%s\n", encoded); err != nil {
			fmt.Fprintf(o.stderr, "asmai: %v\n", err)
			return 1
		}
		return 0
	}
	enc := json.NewEncoder(o.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(o.stderr, "asmai: %v\n", err)
		return 1
	}
	return 0
}

func (o *output) table(rows [][]string) {
	writeTable(o.stdout, rows)
}

func writeTable(out io.Writer, rows [][]string) {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(w, "\t")
			}
			fmt.Fprint(w, cell)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
}

// fail reports err and returns the exit code for it: with --json as a JSON
// object on stdout, with --toon as the same object in TOON, otherwise on
// stderr.
func (o *output) fail(err error) int {
	if o.structured() {
		o.printData(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintf(o.stderr, "asmai: %v\n", err)
	}
	return 1
}

// isFile reports whether w is the file at path.
func isFile(w io.Writer, path string) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	open, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(open, named)
}

// rendered writes the log lines it is given rendered for the user.
type rendered struct {
	w io.Writer
}

func (r *rendered) Write(p []byte) (int, error) {
	for line := range bytes.Lines(p) {
		fmt.Fprintln(r.w, logfile.Format(bytes.TrimSuffix(line, []byte("\n"))))
	}
	return len(p), nil
}
