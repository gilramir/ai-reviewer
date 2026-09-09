package review

import (
	"context"
	"fmt"
	"os/exec"
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

	MaxBudgetUSD float64 `json:"maxBudgetUsd"`
	// SpentUSD is what this daemon has spent since it started, summed from what
	// the CLI reports for each turn. It is not a bill: a resumed conversation
	// from a previous run starts this count again at zero.
	SpentUSD float64 `json:"spentUsd"`

	// Live is how many CLI processes are running right now.
	Live int `json:"live"`

	CLIPath    string `json:"cliPath"`
	CLIVersion string `json:"cliVersion"`
}

// modelChoices are the aliases the UI offers. An empty string is "whatever the
// reviewer's own configuration says", which is the right default for a tool
// that deliberately has no API key of its own.
var modelChoices = []string{"", "opus", "sonnet", "haiku", "fable"}

// Settings reports the current configuration.
func (r *Review) Settings() Settings {
	cfg := r.procs.Config()

	r.mu.Lock()
	running := r.runningModel
	spent := r.spentUSD
	r.mu.Unlock()

	choices := append([]string(nil), modelChoices...)
	// A model pinned on the command line that is not one of the aliases still
	// has to be selectable, or opening the panel would silently offer to change
	// it and never offer to change it back.
	if cfg.Model != "" && !contains(choices, cfg.Model) {
		choices = append(choices, cfg.Model)
	}

	return Settings{
		Model:          cfg.Model,
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
		CLIPath:        cfg.Binary,
		CLIVersion:     r.cliVersion,
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
	return nil
}

// noteTurnCost records what a finished turn cost and which model ran it.
func (r *Review) noteTurnCost(model string, costUSD float64) {
	r.mu.Lock()
	if model != "" {
		r.runningModel = model
	}
	r.spentUSD += costUSD
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
