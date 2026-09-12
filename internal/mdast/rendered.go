package mdast

import "strings"

// Rendered is a document's text as the browser shows it, with the source
// position of every run that has one.
//
// It exists so that locating a passage is a search through the words the
// reviewer actually selected from, rather than through the Markdown those words
// were rendered from. The two are not the same string: `**[a link](x.md)** and
// on` reaches the screen as `a link and on`, and no amount of skipping syntax
// characters in the source turns one into the other -- a link's destination is
// ordinary text that the renderer consumed, and so are an image's alt, an
// entity, and the inside of an HTML tag.
//
// The text is built by the same rules the client's Doc.text uses, from the same
// tree, so both halves of the system search the same string.
type Rendered struct {
	Text string
	Runs []Run
	// Blocks is where each top-level element of the document fell in Text. It
	// is what lets a reviewing pass hand a model one section of the document
	// and still be searching the same string the anchors are found in.
	Blocks []Block
}

// Block is one top-level element of a document and the stretch of rendered text
// it produced.
type Block struct {
	Kind  Kind
	Level int    // heading level; 0 for everything else
	Title string // a heading's own rendered text; empty otherwise

	TextStart int
	TextEnd   int
}

// Run is one stretch of the rendered text and where it came from.
//
// Start and End are -1 for a run the parser cannot place: the label of an
// autolink, whose segment goldmark keeps to itself, and the space a line break
// stands for. A passage that begins or ends inside one of those is anchored to
// the edge of its neighbour instead, which is a byte or two wide of the mark and
// never wrong about which passage it is.
type Run struct {
	TextStart int
	TextEnd   int
	Start     int
	End       int
	// Exact records that this run's text is the source, byte for byte, so an
	// offset inside it can be mapped by adding. An indented code block is the
	// counter-example: the source carries an indent the rendered text does not.
	Exact bool
}

// Flatten renders a document to text and records where each run came from.
func Flatten(src []byte) Rendered {
	doc := Render("", 0, src)

	f := flattener{src: src}
	f.top(doc.Root)
	return Rendered{Text: f.text.String(), Runs: f.runs, Blocks: f.blocks}
}

type flattener struct {
	src    []byte
	text   strings.Builder
	runs   []Run
	blocks []Block
}

// top walks the document and notes where each of its top-level children began
// and ended in the text.
//
// The offsets are taken during the walk that builds the text rather than
// measured off the finished string afterwards. Measuring afterwards would mean
// a second traversal counting characters, and a second traversal is a second
// opinion about the one string this file exists to be the only source of.
func (f *flattener) top(root Node) {
	f.emit(ownText(root), root.Span)

	for _, child := range root.Children {
		start := f.text.Len()
		f.walk(child)
		f.blocks = append(f.blocks, Block{
			Kind:      child.Kind,
			Level:     child.Level,
			Title:     headingTitle(child),
			TextStart: start,
			TextEnd:   f.text.Len(),
		})
	}

	if isBlock(root.Kind) {
		f.separate()
	}
}

// headingTitle is a heading's text, for naming the section it opens. Empty for
// everything that is not a heading, which is how a section without one is told
// apart from a section whose heading happens to be blank.
func headingTitle(node Node) string {
	if node.Kind != KindHeading {
		return ""
	}
	var b strings.Builder
	var walk func(Node)
	walk = func(n Node) {
		b.WriteString(ownText(n))
		for _, child := range n.Children {
			walk(child)
		}
	}
	walk(node)
	return strings.TrimSpace(b.String())
}

func (f *flattener) walk(node Node) {
	f.emit(ownText(node), node.Span)

	for _, child := range node.Children {
		f.walk(child)
	}

	// Blocks are separate elements on screen, and a selection crossing two of
	// them arrives with a newline in it that the concatenated text would not
	// otherwise have. One newline is enough: whitespace is compared as runs.
	if isBlock(node.Kind) {
		f.separate()
	}
}

// ownText is what a node contributes before its children, and it has to agree
// with the client's Doc.text exactly -- that is the whole point of building it
// from the same tree.
func ownText(node Node) string {
	switch node.Kind {
	case KindText, KindCodeSpan, KindCodeBlock, KindHTMLBlock, KindRawHTML:
		return node.Text
	case KindLineBreak:
		return " "
	default:
		return ""
	}
}

func isBlock(kind Kind) bool {
	switch kind {
	case KindDocument, KindParagraph, KindHeading, KindCodeBlock, KindBlockQuote,
		KindList, KindListItem, KindThematicBreak, KindHTMLBlock,
		KindTable, KindTableRow, KindTableCell:
		return true
	}
	return false
}

func (f *flattener) emit(text string, span Span) {
	if text == "" {
		return
	}

	run := Run{
		TextStart: f.text.Len(),
		TextEnd:   f.text.Len() + len(text),
		Start:     -1,
		End:       -1,
	}
	if span.End > span.Start && span.End <= len(f.src) {
		run.Start, run.End = span.Start, span.End
		run.Exact = string(f.src[span.Start:span.End]) == text
	}

	f.text.WriteString(text)
	f.runs = append(f.runs, run)
}

// separate ends a block with a newline, unless the text already ends in
// whitespace or has not started.
func (f *flattener) separate() {
	current := f.text.String()
	if current == "" || isSpaceByte(current[len(current)-1]) {
		return
	}
	f.emit("\n", Span{})
}

// Source maps a range of the rendered text back to a range of the source.
//
// The result is the smallest source range that certainly contains the passage:
// a run that cannot be mapped precisely contributes its whole extent rather
// than a guess at an offset inside it.
func (r Rendered) Source(textStart, textEnd int) (int, int, bool) {
	if textStart >= textEnd {
		return 0, 0, false
	}

	start, ok := r.sourceStart(textStart)
	if !ok {
		return 0, 0, false
	}
	end, ok := r.sourceEnd(textEnd)
	if !ok || end <= start {
		return 0, 0, false
	}
	return start, end, true
}

// sourceStart is where the run holding this offset begins in the source, or
// where the next placeable run does when this one cannot be placed.
func (r Rendered) sourceStart(at int) (int, bool) {
	index := r.runAt(at)
	if index < 0 {
		return 0, false
	}

	for i := index; i < len(r.Runs); i++ {
		run := r.Runs[i]
		if run.Start < 0 {
			continue
		}
		if i == index && run.Exact {
			return run.Start + (at - run.TextStart), true
		}
		return run.Start, true
	}
	return 0, false
}

// sourceEnd is where the run holding the last character ends, or where the
// previous placeable run does.
func (r Rendered) sourceEnd(at int) (int, bool) {
	index := r.runAt(at - 1)
	if index < 0 {
		return 0, false
	}

	for i := index; i >= 0; i-- {
		run := r.Runs[i]
		if run.End < 0 {
			continue
		}
		if i == index && run.Exact {
			return run.Start + (at - run.TextStart), true
		}
		return run.End, true
	}
	return 0, false
}

func (r Rendered) runAt(offset int) int {
	// Runs are in order and do not overlap, so a walk is a scan of a list that
	// is short by construction: one entry per text node in the document.
	for i, run := range r.Runs {
		if offset >= run.TextStart && offset < run.TextEnd {
			return i
		}
	}
	return -1
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
