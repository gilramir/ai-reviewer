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

	filed, lost := 0, 0
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

		n, missed, err := r.reviewSection(ctx, session, docPath, brief, rendered, section)
		filed += n
		lost += missed
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.publish(errorFrame{Type: "error", Message: "review of " + docPath + " stopped: " + err.Error()})
			break
		}
	}

	// Reported once for the pass rather than once per section: three sections
	// that each lost one comment is one number the reviewer can act on, and
	// three notices they will dismiss without reading.
	if lost > 0 {
		r.publish(errorFrame{Type: "error", Message: fmt.Sprintf(
			"%d comment(s) on %s quoted text that is not in the document, and were dropped even after a second look.",
			lost, docPath)})
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

// reviewSection runs one turn, gives the model a second look at whatever it
// misquoted, and files what survives. It returns how many threads that was and
// how many comments were lost.
//
// The second look is the point. A comment whose passage cannot be found is
// thrown away, and a thrown-away comment is indistinguishable from one that was
// never raised -- so the reviewer never learns the pass had something to say.
// One extra turn, in the conversation that still has the section in it, turns
// most of those back into comments: the usual failure is a word, not an
// invention. "the reviewer has selected" for "the reviewer selected" is a
// hallucination the model can fix the moment it is shown where it diverged.
//
// One round and no more. If the text was not there the second time either, it
// was never there, and a third ask is the model repeating itself at full price.
func (r *Review) reviewSection(ctx context.Context, session *claudeproc.Session, docPath, brief string, rendered mdast.Rendered, section mdast.Section) (filed int, lost int, err error) {
	gate, err := r.newGate(docPath, brief, rendered, section)
	if err != nil {
		return 0, 0, err
	}

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
		return 0, 0, err
	}

	bad := gate.admitAll(parseProposals(result.Text))
	lost = len(bad)

	if lost > 0 && ctx.Err() == nil {
		before := len(gate.kept)

		repaired, askErr := session.Ask(turn, repairPrompt(bad), nil)
		r.noteTurnCost(docPath, repaired.Model, repaired.CostUSD)

		// Counted as what the gate gained, not as what the second turn sent
		// back. A repair that comes back misquoted again, and one the model
		// ignores entirely, are the same outcome -- the comment is gone -- and
		// measuring the reply rather than the result would score the second as
		// a success. A failed turn is the same again: the first turn's comments
		// stand, and the ones that needed a second look are lost.
		if askErr == nil {
			gate.admitAll(parseProposals(repaired.Text))
		}
		if fixed := len(gate.kept) - before; fixed < lost {
			lost -= fixed
		} else {
			lost = 0
		}
	}

	return r.fileVetted(docPath, brief, gate.kept), lost, nil
}

// vetted is a proposal that passed the gate: the anchor to file it under, and
// where that sits in the text the model was shown. The Location is in
// rendered-text coordinates and is only ever compared with another of its own
// kind -- see AnchorIn.
type vetted struct {
	anchor  Anchor
	comment string
	at      Location
}

// rejected is a proposal that did not pass, in the words the model gets back.
type rejected struct {
	quote  string
	reason string
}

// sectionGate admits proposals for one section.
//
// It holds the document twice on purpose, and they are not interchangeable:
// rendered is what the model was shown and is where a quote has to be found,
// and current is what is on disk now and is where the resulting anchor has to
// still work. An editing turn or the reviewer's own hand can have moved the
// text in between.
type sectionGate struct {
	rev      *Review
	docPath  string
	brief    string
	rendered mdast.Rendered
	section  mdast.Section
	current  mdast.Rendered

	kept []vetted
}

func (r *Review) newGate(docPath, brief string, rendered mdast.Rendered, section mdast.Section) (*sectionGate, error) {
	src, err := r.read(docPath)
	if err != nil {
		return nil, err
	}
	return &sectionGate{
		rev:      r,
		docPath:  docPath,
		brief:    brief,
		rendered: rendered,
		section:  section,
		current:  mdast.Flatten(src),
	}, nil
}

// admitAll runs a batch through the gate and returns what it would not take.
func (g *sectionGate) admitAll(proposals []proposal) []rejected {
	var bad []rejected
	for _, p := range proposals {
		if why, ok := g.admit(p); !ok {
			bad = append(bad, why)
		}
	}
	return bad
}

// admit checks one proposal, keeping it when it passes.
//
// A refusal it can do something about comes back as a rejection with a reason;
// a passage that is simply already being discussed comes back as neither, since
// asking for that one again is asking it to widen the quote until the check
// stops noticing.
func (g *sectionGate) admit(p proposal) (rejected, bool) {
	comment := strings.TrimSpace(p.Comment)
	quote := strings.TrimSpace(p.Quote)

	if comment == "" {
		return rejected{quote: quote, reason: "there was no comment with it."}, false
	}
	if len(quote) < quoteFloor {
		return rejected{quote: quote, reason: fmt.Sprintf(
			"that is too short to point at one place. Quote a whole phrase -- %d characters at least, a sentence for preference.",
			quoteFloor)}, false
	}

	anchor, at, ok := AnchorIn(g.rendered, g.section, quote)
	if !ok {
		return rejected{quote: quote, reason: g.missing(quote)}, false
	}
	if _, ok := LocateIn(g.current, anchor); !ok {
		return rejected{quote: quote, reason: "that passage was in the section but is not in the file any more -- it changed while I was reading."}, false
	}

	if g.rev.overlaps(g.docPath, g.brief, g.rendered, at) || overlapsVetted(g.kept, at) {
		return rejected{}, true
	}

	g.kept = append(g.kept, vetted{anchor: anchor, comment: comment, at: at})
	return rejected{}, true
}

// missing says where a quote stopped matching the section it was supposed to be
// copied out of.
//
// "Not found" is a coin flip to retry against: the model has no way to tell
// whether it invented the passage or mistyped a word in it. Being shown the
// longest piece that did match, and what the text actually says from there,
// makes the usual case a one-word correction.
func (g *sectionGate) missing(quote string) string {
	prefix, at, ok := longestPrefixIn(g.rendered, g.section, quote)
	if !ok {
		return "no part of that is in the section. Quote from the text in my message, not from the file."
	}

	// As much of the real text as the quote claimed to be, so the difference is
	// visible rather than described.
	end := at.Start + len(quote) + 16
	if end > g.section.End {
		end = g.section.End
	}
	if end > len(g.rendered.Text) {
		end = len(g.rendered.Text)
	}

	return fmt.Sprintf("it matches as far as %q, and the text there reads: %q",
		oneLine(prefix), oneLine(toWordEnd(g.rendered.Text[at.Start:end])))
}

// toWordEnd trims a excerpt back to its last whole word.
//
// The excerpt is there to be copied out of, and one ending mid-word invites a
// requote that ends mid-word too -- which anchors, technically, and highlights
// half a sentence. Left alone if there is no space to trim to, since a cut is
// better than nothing to show.
func toWordEnd(s string) string {
	if at := strings.LastIndexAny(s, " \t\n"); at > len(s)/2 {
		return s[:at]
	}
	return s
}

// longestPrefixIn finds the longest opening piece of a quote that is still in
// the section, and one place it occurs.
//
// Walking up rather than down, and stopping at the first miss: if a prefix is
// not in the text then nothing longer that starts with it can be either, so the
// first failure is the answer.
func longestPrefixIn(rendered mdast.Rendered, section mdast.Section, quote string) (string, Location, bool) {
	best, at, found := "", Location{}, false

	for _, end := range runeEnds(quote) {
		where, ok := firstIn(rendered.Text, quote[:end], section.Start, section.End)
		if !ok {
			break
		}
		best, at, found = quote[:end], where, true
	}
	return best, at, found
}

// runeEnds lists the offsets a string can be cut at without splitting a rune.
func runeEnds(s string) []int {
	var ends []int
	for i := range s {
		if i > 0 {
			ends = append(ends, i)
		}
	}
	if len(s) > 0 {
		ends = append(ends, len(s))
	}
	return ends
}

func overlapsVetted(kept []vetted, at Location) bool {
	for _, v := range kept {
		if v.at.Start < at.End && at.Start < v.at.End {
			return true
		}
	}
	return false
}

// fileVetted turns what the gate kept into threads and tells the browser.
func (r *Review) fileVetted(docPath, brief string, kept []vetted) int {
	if len(kept) == 0 {
		return 0
	}

	r.mu.Lock()
	for _, v := range kept {
		thread := &Thread{
			ID:       uuid.NewString(),
			Doc:      docPath,
			Anchor:   v.anchor,
			Status:   StatusOpen,
			Origin:   OriginModel,
			Brief:    brief,
			Messages: []Message{{Role: RoleAssistant, Text: v.comment}},
			Created:  time.Now().UTC(),
		}
		r.threads[thread.ID] = thread
		r.order = append(r.order, thread.ID)
	}
	r.mu.Unlock()

	r.broadcastThreads(docPath)
	return len(kept)
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

// parseProposals pulls the comments out of a turn's final text.
//
// The model is asked for a fenced JSON array and mostly sends one, wrapped in
// however much prose it felt like. Every fenced block is tried and the last one
// that parses wins, because a model that shows an example before its answer
// puts the answer second; the bare text is tried too, for the turn that sent
// nothing but the array.
//
// A model that ignores the format entirely sends prose and nothing parses, and
// that is a real outcome rather than an error: the turn raised nothing. It is
// the repair round, not this, that gets a misquote looked at again.
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
