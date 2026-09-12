package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/gilramir/ai-reviewer/internal/claudeproc"
	"github.com/gilramir/ai-reviewer/internal/mdast"
)

// A reviewing pass is the other direction of this tool: the model reads a
// document and raises comments on it, and the reviewer answers them.
//
// It reuses the thread wholesale. A thread is an anchor and a transcript, and
// it does not care who wrote the first message -- the highlight, the
// re-anchoring after an edit, the outdating of a passage that was rewritten and
// the click from a passage to the conversation about it already work on any
// anchored thread. What is new is where the anchor comes from, and that decides
// everything else here.

// sectionCap is how much rendered text a single turn is given, in bytes. It is
// about two thousand words: enough that a section is rarely split, small enough
// that a quote is being looked for in a page rather than a book.
const sectionCap = 12000

// quoteFloor is the shortest quote a proposal may carry.
//
// A reviewer selects a passage they can see, which is almost always long enough
// to occur once. A model picks the words it is talking about, and "the reviewer"
// is three of them. Below this the anchor is not wrong so much as arbitrary:
// it will land somewhere the phrase occurs, and the reviewer has no way to know
// it was not the place that was meant.
const quoteFloor = 24

// briefFile is where a review's standing brief lives, when it has one.
//
// A file rather than a field in the settings panel, for two reasons. Settings
// is assembled on demand precisely because every field in it already has an
// owner elsewhere, and a brief kept there would be the second copy that design
// exists to avoid. And a brief is a thing to edit, diff and commit beside the
// documents it governs, which is how this repository already carries its
// conventions.
const briefFile = "review.md"

// Brief is what a pass looks for unless the reviewer says otherwise.
func (r *Review) Brief() string {
	data, err := os.ReadFile(filepath.Join(r.work, ".ai-reviewer", briefFile))
	if err != nil {
		return defaultBrief
	}
	if text := strings.TrimSpace(string(data)); text != "" {
		return text
	}
	return defaultBrief
}

// proposal is one comment as the model offers it, before anything has checked
// that it is about a passage that exists.
type proposal struct {
	Quote   string `json:"quote"`
	Comment string `json:"comment"`
}

// StartPass reviews a document, one section at a time, filing a thread for
// every comment whose passage can be found.
//
// An empty brief takes the standing one. The pass runs in the background: it is
// several turns long and the reviewer is meant to start reading the first
// section's comments while the rest is still going.
func (r *Review) StartPass(docPath, brief string) error {
	src, err := r.read(docPath)
	if err != nil {
		return err
	}

	brief = strings.TrimSpace(brief)
	if brief == "" {
		brief = r.Brief()
	}

	rendered := mdast.Flatten(src)
	sections := rendered.Sections(sectionCap)
	if len(sections) == 0 {
		return fmt.Errorf("%s has nothing to review", docPath)
	}

	ctx, cancel := context.WithCancel(context.Background())

	r.mu.Lock()
	if _, running := r.passes[docPath]; running {
		r.mu.Unlock()
		cancel()
		return fmt.Errorf("a review of %s is already running", docPath)
	}
	r.passes[docPath] = cancel
	r.mu.Unlock()

	r.pending.Add(1)
	go r.runPass(ctx, docPath, brief, rendered, sections)
	return nil
}

// PassRunning reports whether a reviewing pass is working on a document.
func (r *Review) PassRunning(docPath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, running := r.passes[docPath]
	return running
}

// runPass walks the sections. Every caller counts it into r.pending first, and
// it is this function's job to count it out.
func (r *Review) runPass(ctx context.Context, docPath, brief string, rendered mdast.Rendered, sections []mdast.Section) {
	defer r.pending.Done()
	defer func() {
		r.mu.Lock()
		delete(r.passes, docPath)
		r.mu.Unlock()
		r.publishPass(docPath, passProgress{})
		r.PublishSettings()
	}()

	r.setBusy(docPath, 1)
	defer r.setBusy(docPath, -1)

	// One conversation for the whole pass and no longer.
	//
	// One, rather than a conversation per section, because the model that has
	// just read three sections is the one that knows it has already said this.
	// No longer, because the brief is what the conversation was for: a second
	// pass looking for something else should not inherit the first one's
	// opinions, and the sections it is about to be shown are the same sections
	// it already commented on.
	passID := uuid.NewString()
	sessionKey := docPath + "\x00" + passID
	defer r.critics.Forget(sessionKey)
	session := r.critics.Session(sessionKey, passID)

	filed := 0
	for i, section := range sections {
		if ctx.Err() != nil {
			break
		}

		r.publishPass(docPath, passProgress{
			Active:  true,
			Section: i + 1,
			Total:   len(sections),
			Title:   section.Title,
			Brief:   brief,
		})

		n, err := r.reviewSection(ctx, session, docPath, brief, rendered, section)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.publish(errorFrame{Type: "error", Message: "review of " + docPath + " stopped: " + err.Error()})
			break
		}
		filed += n
	}

	// A pass that raised nothing has to say so. Silence is what a pass that
	// failed looks like, and the reviewer cannot tell the two apart from an
	// empty sidebar.
	if filed == 0 && ctx.Err() == nil {
		r.publish(errorFrame{
			Type:    "error",
			Message: "Nothing to raise in " + docPath + " under that brief.",
		})
	}
	_ = r.save()
}

// reviewSection runs one turn and files what survives it, returning how many
// threads that was.
func (r *Review) reviewSection(ctx context.Context, session *claudeproc.Session, docPath, brief string, rendered mdast.Rendered, section mdast.Section) (int, error) {
	prompt := criticPrompt(
		r.workspacePath(docPath),
		brief,
		section.Title,
		rendered.Slice(section),
		r.quotesTaken(docPath, brief, rendered, section),
	)

	turn, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	result, err := session.Ask(turn, prompt, nil)
	r.noteTurnCost(docPath, result.Model, result.CostUSD)
	if err != nil {
		return 0, err
	}

	return r.fileProposals(docPath, brief, rendered, section, parseProposals(result.Text)), nil
}

// quotesTaken lists the passages in a section that are already being discussed,
// so the pass does not raise them again.
//
// A reviewer's own open thread counts whatever it was about: a passage they are
// already arguing over is the last place a machine comment helps. A machine
// thread counts only against the same brief, because two comments on one
// sentence for two different reasons are two comments, legitimately.
func (r *Review) quotesTaken(docPath, brief string, rendered mdast.Rendered, section mdast.Section) []string {
	var out []string

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, id := range r.order {
		thread := r.threads[id]
		if thread == nil || thread.Doc != docPath || thread.Status != StatusOpen {
			continue
		}
		if thread.Origin == OriginModel && thread.Brief != brief {
			continue
		}
		if _, _, ok := AnchorIn(rendered, section, thread.Anchor.Quote); ok {
			out = append(out, thread.Anchor.Quote)
		}
	}
	return out
}

// fileProposals turns what the model offered into threads, dropping whatever
// cannot be anchored.
//
// The gate is the point of the whole design. A comment whose passage cannot be
// found is not a comment; it is a remark about a document with nowhere to put
// it, and showing it anyway would put a thread on screen that no highlight
// corresponds to. Every proposal is therefore found in the section it was taken
// from, given the context around it, and then located again in the file exactly
// the way every later re-anchoring will locate it. Anything that fails is
// counted and reported, never guessed at.
func (r *Review) fileProposals(docPath, brief string, rendered mdast.Rendered, section mdast.Section, proposals []proposal) int {
	if len(proposals) == 0 {
		return 0
	}

	// Read again rather than reusing the pass's copy: an editing turn or the
	// reviewer's own hand may have changed the file since the pass started, and
	// a thread anchored to text that is no longer there is born outdated.
	src, err := r.read(docPath)
	if err != nil {
		return 0
	}
	current := mdast.Flatten(src)

	var filed []*Thread
	dropped := 0

	for _, p := range proposals {
		comment := strings.TrimSpace(p.Comment)
		quote := strings.TrimSpace(p.Quote)
		if comment == "" || len(quote) < quoteFloor {
			dropped++
			continue
		}

		anchor, at, ok := AnchorIn(rendered, section, quote)
		if !ok {
			dropped++
			continue
		}
		if _, ok := LocateIn(current, anchor); !ok {
			dropped++
			continue
		}
		if r.overlaps(docPath, brief, rendered, at) || overlapsFiled(rendered, filed, at) {
			continue
		}

		filed = append(filed, &Thread{
			ID:       uuid.NewString(),
			Doc:      docPath,
			Anchor:   anchor,
			Status:   StatusOpen,
			Origin:   OriginModel,
			Brief:    brief,
			Messages: []Message{{Role: RoleAssistant, Text: comment}},
			Created:  time.Now().UTC(),
		})
	}

	if len(filed) > 0 {
		r.mu.Lock()
		for _, thread := range filed {
			r.threads[thread.ID] = thread
			r.order = append(r.order, thread.ID)
		}
		r.mu.Unlock()
		r.broadcastThreads(docPath)
	}

	if dropped > 0 {
		r.publish(errorFrame{Type: "error", Message: fmt.Sprintf(
			"%d comment(s) on %s quoted text that is not in the document and were dropped.",
			dropped, docPath)})
	}
	return len(filed)
}

// overlaps reports that an existing thread already covers this passage. Same
// rule as quotesTaken, applied to what the model sent back rather than to what
// it was told.
func (r *Review) overlaps(docPath, brief string, rendered mdast.Rendered, at Location) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, id := range r.order {
		thread := r.threads[id]
		if thread == nil || thread.Doc != docPath || thread.Status != StatusOpen {
			continue
		}
		if thread.Origin == OriginModel && thread.Brief != brief {
			continue
		}
		if other, ok := firstIn(rendered.Text, strings.TrimSpace(thread.Anchor.Quote), 0, len(rendered.Text)); ok {
			if other.Start < at.End && at.Start < other.End {
				return true
			}
		}
	}
	return false
}

func overlapsFiled(rendered mdast.Rendered, filed []*Thread, at Location) bool {
	for _, thread := range filed {
		if other, ok := firstIn(rendered.Text, strings.TrimSpace(thread.Anchor.Quote), 0, len(rendered.Text)); ok {
			if other.Start < at.End && at.Start < other.End {
				return true
			}
		}
	}
	return false
}

// parseProposals pulls the comments out of a turn's final text.
//
// The model is asked for a fenced JSON array and mostly sends one, wrapped in
// however much prose it felt like. Every fenced block is tried and the last one
// that parses wins, because a model that shows an example before its answer
// puts the answer second; the bare text is tried too, for the turn that sent
// nothing but the array.
//
// This is the cheap half of the design. The proper version is a tool the daemon
// answers, called once per comment, which can say "that passage is not in the
// section I gave you" while the model is still in a position to fix it. Until
// then a bad quote is silently expensive, which is what the dropped count in
// fileProposals exists to make visible.
func parseProposals(text string) []proposal {
	var best []proposal
	found := false

	for _, block := range fencedBlocks(text) {
		if parsed, ok := decodeProposals(block); ok {
			best, found = parsed, true
		}
	}
	if !found {
		if parsed, ok := decodeProposals(text); ok {
			best = parsed
		}
	}
	return best
}

func decodeProposals(text string) ([]proposal, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}
	var out []proposal
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, false
	}
	return out, true
}

// fencedBlocks returns the contents of every ``` fence in a string. The text is
// the model's prose rather than a document, so this is deliberately not a
// Markdown parse: an unterminated fence at the end is still worth reading.
func fencedBlocks(text string) []string {
	lines := strings.Split(text, "\n")

	var out []string
	var current []string
	inside := false

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inside {
				out = append(out, strings.Join(current, "\n"))
				current = nil
			}
			inside = !inside
			continue
		}
		if inside {
			current = append(current, line)
		}
	}
	if inside && len(current) > 0 {
		out = append(out, strings.Join(current, "\n"))
	}
	return out
}
