// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/talvor/asmai"
	"github.com/talvor/asmai/internal/daemon"
	"github.com/talvor/asmai/internal/statedir"
	"github.com/talvor/asmai/internal/store"
)

// agentCommands are the commands an agent session may run.
var agentCommands = []string{"status", "agents", "jobs", "brief", "job open", "hook", "inbox", "handoff send", "handoff accept", "handoff clarify", "handoff decline", "assign", "effect", "result", "blocked", "accept", "reject", "cancel", "push", "decision record"}

// userCommands are the commands only the user runs, which an agent is told
// to ask the user for.
var userCommands = []string{"setup", "start", "stop", "providers install", "providers list", "repo add", "repo list", "repo show", "repo remove"}

// usageError is a command line the command tree refuses: the exit code is 2,
// and the message is followed by the usage.
type usageError struct {
	command string // "" for asmai itself
	message string
}

func (e *usageError) Error() string { return e.message }

// invocation is one run of asmai: what its commands read and write, and the
// exit code the command that ran returned.
type invocation struct {
	original       []string
	stdin          io.Reader
	stdout, stderr io.Writer
	jsonOutput     bool
	code           int
}

const nameAnnotation = "asmai/name"

// commandName is the name a command is known by in messages, such as
// "repo add".
func commandName(cmd *cobra.Command) string {
	if name, ok := cmd.Annotations[nameAnnotation]; ok {
		return name
	}
	return strings.TrimPrefix(cmd.CommandPath(), "asmai ")
}

// operandArgs checks a command's operands: each of wants names one that is
// missing, in order.
func operandArgs(name string, wants []string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < len(wants) {
			return &usageError{name, wants[len(args)]}
		}
		if len(args) > len(wants) {
			return &usageError{name, fmt.Sprintf("unexpected argument %q", args[len(wants)])}
		}
		return nil
	}
}

// leaf is a command that runs one function with the state directory's paths
// and its operands.
type leaf struct {
	name     string
	operands []string
	flags    func(*pflag.FlagSet)
	run      func(o *output, paths statedir.Paths, operands []string) int
}

func (in *invocation) leaf(l leaf) *cobra.Command {
	words := strings.Fields(l.name)
	cmd := &cobra.Command{
		Use:         words[len(words)-1],
		Annotations: map[string]string{nameAnnotation: l.name},
		Args:        operandArgs(l.name, l.operands),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.code = in.execute(l, args)
			return nil
		},
	}
	if l.flags != nil {
		l.flags(cmd.Flags())
	}
	return cmd
}

func (in *invocation) execute(l leaf, args []string) int {
	o := &output{stdout: in.stdout, stderr: in.stderr, json: in.jsonOutput}
	if os.Getenv(daemon.SessionCredential) != "" && !slices.Contains(agentCommands, l.name) {
		if slices.Contains(userCommands, l.name) {
			return o.fail(fmt.Errorf("an agent cannot run this command; ask the user to run `asmai %s`", strings.Join(in.original, " ")))
		}
		return o.fail(fmt.Errorf("an agent cannot run asmai %s; use an agent coordination command", l.name))
	}
	switch l.name {
	case "version":
		if o.json {
			return o.printJSON(map[string]string{"version": version})
		}
		fmt.Fprintf(in.stdout, "asmai %s\n", version)
		return 0
	case "notices":
		if o.json {
			return o.printJSON(map[string]string{"license": string(asmai.License), "third_party_notices": string(asmai.ThirdPartyNotices)})
		}
		in.stdout.Write(asmai.License)
		fmt.Fprintln(in.stdout)
		in.stdout.Write(asmai.ThirdPartyNotices)
		return 0
	}
	paths, err := statedir.Default()
	if err != nil {
		return o.fail(err)
	}
	return l.run(o, paths, args)
}

// group is a command grouped by noun: it only has subcommands, and says
// which of them it wants when it is given none or another.
func (in *invocation) group(name string, subcommands ...*cobra.Command) *cobra.Command {
	names := make([]string, len(subcommands))
	for i, c := range subcommands {
		names[i] = c.Name()
	}
	want := names[len(names)-1]
	if len(names) > 1 {
		want = strings.Join(names[:len(names)-1], ", ") + " or " + want
	}
	cmd := &cobra.Command{
		Use:                name,
		Args:               cobra.ArbitraryArgs,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			return &usageError{name, "want " + want}
		},
	}
	cmd.AddCommand(subcommands...)
	return cmd
}

// newRoot builds asmai's command tree for one run.
func (in *invocation) newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:  "asmai",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return &usageError{"", fmt.Sprintf("unknown command %q", args[0])}
		},
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetHelpCommand(&cobra.Command{Use: "no-help", Hidden: true})
	root.SetIn(in.stdin)
	root.SetOut(in.stderr)
	root.SetErr(in.stderr)
	root.PersistentFlags().BoolVar(&in.jsonOutput, "json", false, "print JSON instead of tables")
	// Asked for its flags with -h or --help, a command lists them and exits 2.
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(in.stderr, "Usage of asmai %s:\n%s", commandName(cmd), cmd.Flags().FlagUsages())
		in.code = 2
	})
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{commandName(cmd), err.Error()}
	})

	var (
		foreground, follow, assignReadOnly        bool
		repoName                                  string
		message, repository, reading, mandate     string
		criteria                                  criteriaFlags
		handoffJob, handoffID, dispatchID         int64
		recipient, outcome, decisions, evidence   string
		constraints, permissions, answer          string
		assignJob, decidedAssignment, decisionJob int64
		assignOutcome, assignCommit               string
		blockedReason, blockedNeeds               string
		reasons                                   listFlags
		choice, rationale                         string
		resultArgs                                resultFlags
	)

	chatCmd := in.leaf(leaf{name: "chat", run: func(o *output, paths statedir.Paths, _ []string) int {
		return chat(o, paths, in.stdin)
	}})
	chatCmd.Use = "chat"

	jobShowCmd := in.leaf(leaf{name: "job show", operands: []string{"name the job number, as `asmai jobs` shows it"}, run: func(o *output, paths statedir.Paths, a []string) int {
		return jobShow(o, paths, a[0])
	}})
	// asmai job <number> shows a job; asmai job open opens one.
	jobCmd := jobShowCmd
	jobCmd.Use = "job"
	jobCmd.AddCommand(in.leaf(leaf{name: "job open",
		flags: func(f *pflag.FlagSet) {
			f.StringVar(&message, "message", "", "latest or the witnessed message's journal ID")
			f.StringVar(&repository, "repository", "", "registered repository name")
			f.StringVar(&reading, "reading", "", "Coordination's reading of the user's words")
			f.StringVar(&mandate, "mandate", store.MandateTestedPR, "job mandate")
			f.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
		},
		run: func(o *output, paths statedir.Paths, _ []string) int {
			return jobOpen(o, paths, message, repository, reading, mandate, criteria)
		}}))

	gitHookCmd := &cobra.Command{
		Use:                "git-hook",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.code = gitHook(args, in.stdin, in.stdout, in.stderr)
			return nil
		},
	}

	root.AddCommand(
		chatCmd,
		in.leaf(leaf{name: "setup", run: func(o *output, paths statedir.Paths, _ []string) int {
			return setup(o, paths, in.stdin)
		}}),
		in.leaf(leaf{name: "start",
			flags: func(f *pflag.FlagSet) {
				f.BoolVar(&foreground, "foreground", false, "keep the daemon attached to this terminal")
			},
			run: func(o *output, paths statedir.Paths, _ []string) int {
				if foreground {
					return startForeground(o, paths)
				}
				return start(o, paths)
			}}),
		in.leaf(leaf{name: "stop", run: func(o *output, paths statedir.Paths, _ []string) int { return stop(o, paths) }}),
		in.leaf(leaf{name: "status", run: func(o *output, paths statedir.Paths, _ []string) int { return status(o, paths) }}),
		in.leaf(leaf{name: "agents", run: func(o *output, paths statedir.Paths, _ []string) int { return agents(o, paths) }}),
		in.leaf(leaf{name: "jobs", run: func(o *output, paths statedir.Paths, _ []string) int { return jobs(o, paths) }}),
		jobCmd,
		in.leaf(leaf{name: "brief", operands: []string{"name the job number, as `asmai jobs` shows it"}, run: func(o *output, paths statedir.Paths, a []string) int {
			return jobBrief(o, paths, a[0])
		}}),
		in.leaf(leaf{name: "inbox",
			flags: func(f *pflag.FlagSet) { f.Int64Var(&dispatchID, "dispatch", 0, "fetch one dispatch") },
			run: func(o *output, paths statedir.Paths, _ []string) int {
				return inbox(o, paths, dispatchID)
			}}),
		in.group("handoff",
			in.leaf(leaf{name: "handoff send",
				flags: func(f *pflag.FlagSet) {
					f.Int64Var(&handoffJob, "job", 0, "job number")
					f.StringVar(&recipient, "to", "", "receiving leader")
					f.StringVar(&outcome, "outcome", "", "requested outcome")
					f.StringVar(&decisions, "decisions", "", "relevant decisions")
					f.StringVar(&evidence, "evidence", "", "relevant evidence")
					f.StringVar(&constraints, "constraints", "", "constraints")
					f.StringVar(&permissions, "permissions", "", "permissions granted")
					f.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
				},
				run: func(o *output, paths statedir.Paths, _ []string) int {
					return handoffSend(o, paths, handoffJob, recipient, outcome, decisions, evidence, constraints, permissions, criteria)
				}}),
			in.handoffAnswer("handoff accept", &handoffID, &answer),
			in.handoffAnswer("handoff clarify", &handoffID, &answer),
			in.handoffAnswer("handoff decline", &handoffID, &answer),
		),
		in.leaf(leaf{name: "assign",
			flags: func(f *pflag.FlagSet) {
				f.Int64Var(&assignJob, "job", 0, "job number")
				f.StringVar(&assignOutcome, "outcome", "", "the outcome the worker is to deliver")
				f.Var(&criteria, "criterion", "one acceptance criterion; may be repeated")
				f.BoolVar(&assignReadOnly, "read-only", false, "give a validation, which changes nothing, instead of a writing assignment")
				f.StringVar(&assignCommit, "commit", "", "with --read-only, the job branch's exact head commit the validation is fixed at")
			},
			run: func(o *output, paths statedir.Paths, _ []string) int {
				return assign(o, paths, assignJob, assignOutcome, criteria, assignReadOnly, assignCommit)
			}}),
		in.leaf(leaf{name: "effect",
			operands: []string{"name the kind of effect, one word such as commit or push", "name what the effect is of, such as the commit or the branch pushed"},
			run: func(o *output, paths statedir.Paths, a []string) int {
				return effect(o, paths, a[0], a[1])
			}}),
		in.leaf(leaf{name: "result",
			flags: func(f *pflag.FlagSet) {
				f.Var(&resultArgs.evidence, "evidence", "what shows the work does; may be repeated")
				f.Var(&resultArgs.artifacts, "artifact", "a link to something the work produced; may be repeated")
				f.Var(&resultArgs.tests, "test", "a test added, or none; may be repeated")
				f.Var(&resultArgs.checks, "check", "a check run and its outcome at the commit, '<command> -> <outcome>', or none; may be repeated")
				f.Var(&resultArgs.gaps, "gap", "an unresolved gap, or none; may be repeated")
				f.StringVar(&resultArgs.prSection, "pr-section", "", "the worker's part of the pull request: what changed, with before-and-after evidence")
				f.Var(&resultArgs.findings, "finding", "a validation's finding, '<blocking|advisory|needs-you>: <text>', or none; may be repeated")
				f.Var(&resultArgs.resolved, "resolved", "an earlier blocking finding's ID that is resolved at this commit; may be repeated")
				f.Var(&resultArgs.unresolved, "unresolved", "an earlier blocking finding's ID that is still open at this commit; may be repeated")
			},
			run: func(o *output, paths statedir.Paths, _ []string) int {
				return result(o, paths, resultArgs)
			}}),
		in.leaf(leaf{name: "blocked",
			flags: func(f *pflag.FlagSet) {
				f.StringVar(&blockedReason, "reason", "", "why the worker cannot go on")
				f.StringVar(&blockedNeeds, "needs", "", "what would unblock the worker")
			},
			run: func(o *output, paths statedir.Paths, _ []string) int {
				return blocked(o, paths, blockedReason, blockedNeeds)
			}}),
		in.decided("accept", &decidedAssignment, &reasons),
		in.decided("reject", &decidedAssignment, &reasons),
		in.decided("cancel", &decidedAssignment, &reasons),
		in.leaf(leaf{name: "push", operands: []string{"name the job number, as `asmai jobs` shows it"}, run: func(o *output, paths statedir.Paths, a []string) int {
			return push(o, paths, a[0])
		}}),
		in.group("decision",
			in.leaf(leaf{name: "decision record",
				flags: func(f *pflag.FlagSet) {
					f.Int64Var(&decisionJob, "job", 0, "job number")
					f.StringVar(&choice, "choice", "", "what the decision chose: rerun-failed")
					f.StringVar(&rationale, "rationale", "", "why")
					f.StringVar(&evidence, "evidence", "", "what the decision rests on")
				},
				run: func(o *output, paths statedir.Paths, _ []string) int {
					return decisionRecord(o, paths, decisionJob, choice, rationale, evidence)
				}}),
		),
		in.leaf(leaf{name: "attach", operands: []string{"name the agent, such as leader@coordination or coordination"}, run: func(o *output, paths statedir.Paths, a []string) int {
			return attach(o, paths, a[0], in.stdin)
		}}),
		in.leaf(leaf{name: "log",
			flags: func(f *pflag.FlagSet) {
				f.BoolVar(&follow, "follow", false, "after the log, print each new line as it is written")
			},
			run: func(o *output, paths statedir.Paths, _ []string) int {
				return printLog(o, paths, follow)
			}}),
		in.leaf(leaf{name: "export", run: func(o *output, paths statedir.Paths, _ []string) int { return export(o, paths) }}),
		in.group("providers",
			in.leaf(leaf{name: "providers install", run: func(o *output, paths statedir.Paths, _ []string) int {
				return providersInstall(o, paths, in.stdin)
			}}),
			in.leaf(leaf{name: "providers list", run: func(o *output, paths statedir.Paths, _ []string) int {
				return providersList(o, paths)
			}}),
		),
		in.group("repo",
			in.leaf(leaf{name: "repo add",
				operands: []string{"name a local checkout or a URL"},
				flags: func(f *pflag.FlagSet) {
					f.StringVar(&repoName, "name", "", "register the repository under this name, not one made from its origin")
				},
				run: func(o *output, paths statedir.Paths, a []string) int {
					return repoAdd(o, paths, a[0], repoName, in.stdin)
				}}),
			in.leaf(leaf{name: "repo list", run: func(o *output, paths statedir.Paths, _ []string) int {
				return repoList(o, paths)
			}}),
			in.leaf(leaf{name: "repo show", operands: []string{"name the repository, as `asmai repo list` shows it"}, run: func(o *output, paths statedir.Paths, a []string) int {
				return repoShow(o, paths, a[0])
			}}),
			in.leaf(leaf{name: "repo remove", operands: []string{"name the repository, as `asmai repo list` shows it"}, run: func(o *output, paths statedir.Paths, a []string) int {
				return repoRemove(o, paths, a[0])
			}}),
		),
		in.leaf(leaf{name: "hook", run: func(o *output, paths statedir.Paths, _ []string) int {
			return hook(o, paths, in.stdin)
		}}),
		in.leaf(leaf{name: "version", run: nil}),
		in.leaf(leaf{name: "notices", run: nil}),
		gitHookCmd,
	)
	return root
}

func (in *invocation) handoffAnswer(name string, id *int64, answer *string) *cobra.Command {
	return in.leaf(leaf{name: name,
		flags: func(f *pflag.FlagSet) {
			f.Int64Var(id, "handoff", 0, "handoff ID")
			f.StringVar(answer, "answer", "", "answer, question or reason")
		},
		run: func(o *output, paths statedir.Paths, _ []string) int {
			return handoffAnswer(o, paths, name, *id, *answer)
		}})
}

func (in *invocation) decided(name string, assignment *int64, reasons *listFlags) *cobra.Command {
	return in.leaf(leaf{name: name,
		flags: func(f *pflag.FlagSet) {
			f.Int64Var(assignment, "assignment", 0, "assignment ID")
			f.Var(reasons, "reason", "a reason for the decision; may be repeated")
		},
		run: func(o *output, paths statedir.Paths, _ []string) int {
			return decide(o, paths, name, *assignment, *reasons)
		}})
}
