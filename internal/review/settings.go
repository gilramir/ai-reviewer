package review

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
)

// Settings is what the reviewer can see, and the one thing they can change,
// about the process answering their comments.
//
// It is assembled on demand rather than stored: every field already has an
// owner somewhere — the process manager, the git store, the flags — and a
// second copy would be a second thing to keep true.
type Settings struct {
	// Model is what the daemon asks for. Empty means it asks for nothing, and
	// the CLI uses whatever the reviewer's own configuration selects.
	Model string `json:"model"`
	// RunningModel is what the CLI reported resolving that to, on the most
	// recent turn. It is the answer to "am I on Opus or Sonnet right now?",
	// which the configured value cannot give when it is empty.
	RunningModel string `json:"runningModel"`
	// ModelChoices are the aliases offered in the UI. Aliases rather than exact
	// names because an alias keeps meaning "the current one" as models ship.
	ModelChoices []string `json:"modelChoices"`

	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permissionMode"`

	// Workspace is where Claude runs; ReviewRoot is what the browser may open.
	// They differ whenever a review is rooted inside a repository.
	Workspace  string `json:"workspace"`
	ReviewRoot string `json:"reviewRoot"`
	Branch     string `json:"branch"`
	// BaseBranch is where the review's commits are waiting to go, and Commits
	// is how many are waiting. The count is against the base rather than a
	// tally the daemon keeps, so it goes to zero on its own once they have been
	// merged, and stays right when they are merged from a terminal.
	BaseBranch string `json:"baseBranch"`
	Commits    int    `json:"commits"`
	// Landing is what to run to land them. The daemon runs none of it: a merge
	// can conflict, and a conflict raised inside a review page has nowhere to
	// go. Naming the commands is the useful half.
	Landing Landing `json:"landing"`

	MaxBudgetUSD float64 `json:"maxBudgetUsd"`
	// SpentUSD is what this daemon has spent since it started, summed from what
	// the CLI reports for each turn. It is not a bill: a resumed conversation
	// from a previous run starts this count again at zero.
	SpentUSD float64 `json:"spentUsd"`

	// Live is how many CLI processes are running right now.
	Live int `json:"live"`

	// Conversations is what each document's conversation has grown to since it
	// was last cleared, which is what makes clearing it a decision rather than
	// a guess.
	Conversations []Conversation `json:"conversations"`

	CLIPath    string `json:"cliPath"`
	CLIVersion string `json:"cliVersion"`
}

// Conversation is one document's share of the review.
type Conversation struct {
	Doc      string  `json:"doc"`
	Turns    int     `json:"turns"`
	SpentUSD float64 `json:"spentUsd"`
}

// modelChoices are the aliases the UI offers. An empty string is "whatever the
// reviewer's own configuration says", which is the right default for a tool
// that deliberately has no API key of its own.
var modelChoices = []string{"", "opus", "sonnet", "haiku", "fable"}

// Settings reports the current configuration.
func (r *Review) Settings() Settings {
	cfg := r.procs.Config()
	commits := r.hist.CommitsSince(r.base)

	r.mu.Lock()
	running := r.runningModel
	spent := r.spentUSD
	conversations := make([]Conversation, 0, len(r.turns))
	for docPath, turns := range r.turns {
		conversations = append(conversations, Conversation{
			Doc:      docPath,
			Turns:    turns,
			SpentUSD: r.spend[docPath],
		})
	}
	r.mu.Unlock()

	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].Doc < conversations[j].Doc
	})

	choices := append([]string(nil), modelChoices...)
	// A model pinned on the command line that is not one of the aliases still
	// has to be selectable, or opening the panel would silently offer to change
	// it and never offer to change it back.
	if cfg.Model != "" && !contains(choices, cfg.Model) {
		choices = append(choices, cfg.Model)
	}

	return Settings{
		Model:          cfg.Model,
		BaseBranch:     r.base,
		Commits:        commits,
		Landing:        landingCommands(r.branch, r.base, commits),
		RunningModel:   running,
		ModelChoices:   choices,
		Tools:          cfg.AllowedTools,
		PermissionMode: claudeproc.PermissionMode,
		Workspace:      r.work,
		ReviewRoot:     r.root,
		Branch:         r.branch,
		MaxBudgetUSD:   cfg.MaxBudgetUSD,
		SpentUSD:       spent,
		Live:           r.procs.Live(),
		Conversations:  conversations,
		CLIPath:        cfg.Binary,
		CLIVersion:     r.cliVersion,
	}
}

// Landing is the three commands a finished review ends in. They are spelled
// out rather than left to the reviewer, who is as likely to be a writer as a
// programmer: "your changes are on review/docs-2026-09-09" is not an
// instruction to someone who has never typed git merge.
type Landing struct {
	// Merge brings the commits across as they are, one per turn, each carrying
	// the comment that caused it.
	Merge string `json:"merge"`
	// Squash lands the same final text as a single commit with a message of
	// the reviewer's own, for a review that was one piece of work.
	Squash string `json:"squash"`
	// Delete is what to do with the branch afterwards. It is the lowercase -d,
	// which refuses after a squash -- the one commit that landed is not the
	// commits on the branch, so git cannot tell they arrived. Saying -D here
	// would be saying it before the check that makes it safe.
	Delete string `json:"delete"`
}

// landingCommands spells out how to land a review. It is empty when there is
// nothing to land, or when there is no branch to land it from -- a review
// outside a repository keeps snapshots, which are not merged anywhere.
func landingCommands(branch, base string, commits int) Landing {
	if branch == "" || commits == 0 {
		return Landing{}
	}

	// The branch this was cut from is not known, so the reviewer has to choose
	// one and switch to it themselves; the merges are the same either way.
	switchTo := ""
	if base != "" {
		switchTo = "git switch " + base + " && "
	}

	return Landing{
		Merge:  switchTo + "git merge " + branch,
		Squash: switchTo + "git merge --squash " + branch + " && git commit",
		Delete: "git branch -d " + branch,
	}
}

// SetModel changes the model used from the next turn onwards.
//
// Running processes are not interrupted. Each relaunches when its next comment
// arrives and resumes the same conversation on the new model, so switching
// mid-review costs nothing but keeps everything said so far.
func (r *Review) SetModel(model string) error {
	model = strings.TrimSpace(model)
	if !contains(r.Settings().ModelChoices, model) {
		return fmt.Errorf("unknown model %q", model)
	}

	r.procs.SetModel(model)

	// The old value described a model that is no longer in use, and the new one
	// is not confirmed until a turn reports it.
	r.mu.Lock()
	r.runningModel = ""
	r.mu.Unlock()

	r.PublishSettings()

	// Written now rather than at the end of the next turn: the choice should
	// survive a restart that happens before the reviewer's next comment.
	return r.save()
}

// ClearContext throws away what the model remembers, on every document.
//
// The threads on screen are untouched: they are the review's record, kept by
// the daemon, and nothing about them depends on a process still being alive.
// What goes is the conversation each Claude process is carrying — which is the
// point when the reviewer has just changed model, since a resumed conversation
// arrives at the new model with every word the old one said still in it.
//
// The next comment on a document starts a new conversation, and pays to read
// the document again. That is the cost, and it is the whole cost.
func (r *Review) ClearContext() error {
	r.mu.Lock()
	docs := make([]string, 0, len(r.sessions))
	for docPath := range r.sessions {
		docs = append(docs, docPath)
	}
	r.sessions = map[string]string{}
	// The counts go with the conversations they measure. What was spent stays
	// in the session total: clearing the context does not un-spend it.
	r.turns = map[string]int{}
	r.spend = map[string]float64{}
	r.mu.Unlock()

	for _, docPath := range docs {
		r.procs.Forget(docPath)
	}

	r.PublishSettings()
	return r.save()
}

// noteTurnCost records what a finished turn cost and which model ran it.
//
// Counted per document as well as in total, because the per-document figure is
// the one that answers "is this conversation worth clearing?" -- it is the
// weight the next comment on that document will carry, and it is what Clear
// context sets back to nothing.
func (r *Review) noteTurnCost(docPath, model string, costUSD float64) {
	r.mu.Lock()
	if model != "" {
		r.runningModel = model
	}
	r.spentUSD += costUSD
	r.turns[docPath]++
	r.spend[docPath] += costUSD
	r.mu.Unlock()
}

// claudeVersion asks the CLI what it is. It is one cheap exec at startup — the
// binary answers in milliseconds and talks to nothing — and it turns "which
// claude is this?" from a question into a line in the settings panel.
func claudeVersion(binary string) string {
	if binary == "" {
		binary = "claude"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
