package mdast

import (
	"bytes"
	"sort"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// md is the single parser in the system. The front-end renders whatever this
// produces and never parses Markdown itself.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

// Render parses src and returns a Document ready to be pushed to the browser.
func Render(path string, rev int, src []byte) Document {
	root := md.Parser().Parse(text.NewReader(src))
	c := converter{src: src, newlines: newlineOffsets(src)}
	node, _ := c.convert(root, "n")
	// The parser gives the document node no lines of its own; the document
	// always spans the whole file regardless of what its children cover.
	node.Span = Span{Start: 0, End: len(src)}
	node.Line = 1
	return Document{Path: path, Rev: rev, Root: node}
}

type converter struct {
	src []byte
	// newlines holds the offset of every '\n' in src, so turning a byte offset
	// into a line number is a binary search rather than a rescan per node.
	newlines []int
}

// convert walks one goldmark node into our representation. The bool result is
// false for nodes that should be dropped from the tree entirely (currently only
// GFM task checkboxes, which are hoisted onto their list item instead).
func (c *converter) convert(n ast.Node, id string) (Node, bool) {
	if _, ok := n.(*east.TaskCheckBox); ok {
		return Node{}, false
	}

	out := Node{ID: id}

	switch t := n.(type) {
	case *ast.Document:
		out.Kind = KindDocument
	case *ast.Paragraph:
		out.Kind = KindParagraph
	case *ast.TextBlock:
		// Tight list items hold a TextBlock rather than a Paragraph. The
		// distinction is presentational only, so it does not reach the client.
		out.Kind = KindParagraph
	case *ast.Heading:
		out.Kind, out.Level = KindHeading, t.Level
	case *ast.FencedCodeBlock:
		out.Kind = KindCodeBlock
		out.Lang = string(t.Language(c.src))
		out.Text = c.lineText(t)
	case *ast.CodeBlock:
		out.Kind = KindCodeBlock
		out.Text = c.lineText(t)
	case *ast.Blockquote:
		out.Kind = KindBlockQuote
	case *ast.List:
		out.Kind = KindList
		out.Ordered = t.IsOrdered()
		if t.IsOrdered() && t.Start != 1 {
			out.ListStart = t.Start
		}
	case *ast.ListItem:
		out.Kind = KindListItem
		if checked, ok := taskState(n); ok {
			out.Checked = &checked
		}
	case *ast.ThematicBreak:
		out.Kind = KindThematicBreak
	case *ast.HTMLBlock:
		out.Kind = KindHTMLBlock
		out.Text = c.lineText(t)
	case *ast.Text:
		out.Kind = KindText
		out.Text = string(t.Segment.Value(c.src))
		// A hard or soft break is a line ending the renderer must reproduce;
		// the text itself stops before it.
		if t.HardLineBreak() || t.SoftLineBreak() {
			out.Children = []Node{{Kind: KindLineBreak, ID: id + "-br"}}
		}
	case *ast.String:
		out.Kind = KindText
		out.Text = string(t.Value)
	case *ast.Emphasis:
		if t.Level >= 2 {
			out.Kind = KindStrong
		} else {
			out.Kind = KindEmphasis
		}
	case *east.Strikethrough:
		out.Kind = KindStrikethrough
	case *ast.CodeSpan:
		out.Kind = KindCodeSpan
		out.Text = c.inlineText(t)
	case *ast.Link:
		out.Kind = KindLink
		out.URL, out.Title = string(t.Destination), string(t.Title)
	case *ast.AutoLink:
		out.Kind = KindLink
		url := string(t.URL(c.src))
		out.URL = url
		out.Children = []Node{{Kind: KindText, ID: id + "-t", Text: string(t.Label(c.src))}}
	case *ast.Image:
		out.Kind = KindImage
		out.URL, out.Title = string(t.Destination), string(t.Title)
		out.Alt = c.inlineText(t)
	case *ast.RawHTML:
		out.Kind = KindRawHTML
		out.Text = c.rawHTMLText(t)
	case *east.Table:
		out.Kind = KindTable
	case *east.TableHeader:
		out.Kind = KindTableRow
		out.Header = true
	case *east.TableRow:
		out.Kind = KindTableRow
	case *east.TableCell:
		out.Kind = KindTableCell
		out.Align = alignName(t.Alignment)
		if _, ok := n.Parent().(*east.TableHeader); ok {
			out.Header = true
		}
	default:
		// An unrecognised node still needs to reach the reader, so fall back to
		// its literal source rather than dropping content on the floor.
		out.Kind = KindText
		out.Text = c.lineText(n)
	}

	// Codespan/image/autolink text is already captured above; their goldmark
	// children would duplicate it.
	if out.Kind != KindCodeSpan && out.Kind != KindImage && !isAutoLink(n) {
		out.Children = append(out.Children, c.children(n, id)...)
	}

	out.Span = c.spanOf(n, out.Children)
	out.Line = c.lineOf(n, out.Span)
	return out, true
}

func (c *converter) children(n ast.Node, id string) []Node {
	var kids []Node
	i := 0
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		node, keep := c.convert(ch, id+"-"+strconv.Itoa(i))
		if !keep {
			continue
		}
		kids = append(kids, node)
		i++
	}
	return kids
}

// spanOf resolves a node's byte range. Block nodes carry their own line
// segments; everything else is the union of what its children cover. A node
// with neither (an empty list item, say) reports a zero-width span at its
// parent's start, which the client renders but cannot anchor a comment to.
func (c *converter) spanOf(n ast.Node, kids []Node) Span {
	if seg, ok := lineSpan(n); ok {
		return seg
	}
	if t, ok := n.(*ast.Text); ok {
		return Span{Start: t.Segment.Start, End: t.Segment.Stop}
	}
	if r, ok := n.(*ast.RawHTML); ok && r.Segments.Len() > 0 {
		first, last := r.Segments.At(0), r.Segments.At(r.Segments.Len()-1)
		return Span{Start: first.Start, End: last.Stop}
	}
	var (
		span  Span
		found bool
	)
	for _, k := range kids {
		if k.Span.End == 0 && k.Span.Start == 0 {
			continue
		}
		if !found {
			span, found = k.Span, true
			continue
		}
		if k.Span.Start < span.Start {
			span.Start = k.Span.Start
		}
		if k.Span.End > span.End {
			span.End = k.Span.End
		}
	}
	if found {
		return span
	}

	// A code span keeps its text but not its children -- the client wants one
	// string, not a tree -- so there are no kids here to take a span from. The
	// text is still in the source, in the goldmark node this was built from.
	//
	// goldmark keeps an AutoLink's segment unexported, so those still report a
	// zero span. The enclosing paragraph spans correctly, which is what an
	// anchor falls back to.
	return textSpan(n)
}

// textSpan unions the segments of every literal run under a node.
func textSpan(n ast.Node) Span {
	var (
		span  Span
		found bool
	)
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		var seg Span
		if t, ok := child.(*ast.Text); ok {
			seg = Span{Start: t.Segment.Start, End: t.Segment.Stop}
		} else {
			seg = textSpan(child)
		}
		if seg.End <= seg.Start {
			continue
		}
		if !found {
			span, found = seg, true
			continue
		}
		if seg.Start < span.Start {
			span.Start = seg.Start
		}
		if seg.End > span.End {
			span.End = seg.End
		}
	}
	return span
}

func newlineOffsets(src []byte) []int {
	var offsets []int
	for i := 0; ; {
		j := bytes.IndexByte(src[i:], '\n')
		if j < 0 {
			return offsets
		}
		i += j + 1
		offsets = append(offsets, i-1)
	}
}

// lineOf resolves the 1-based line a node starts on, or 0 for a node with no
// span of its own — an autolink, an empty list item — which the client omits
// rather than mislabelling as line 1.
//
// A fenced code block is the one node whose span is not where the reviewer sees
// it begin: goldmark's lines cover the code, not the fence above it. The margin
// has to agree with what a text editor would show, so the fence wins.
func (c *converter) lineOf(n ast.Node, span Span) int {
	if fenced, ok := n.(*ast.FencedCodeBlock); ok {
		if fenced.Info != nil {
			return c.lineAt(fenced.Info.Segment.Start)
		}
		if span != (Span{}) {
			return c.lineAt(span.Start) - 1
		}
		return 0
	}
	if span == (Span{}) {
		return 0
	}
	return c.lineAt(span.Start)
}

func (c *converter) lineAt(offset int) int {
	return sort.SearchInts(c.newlines, offset) + 1
}

func lineSpan(n ast.Node) (Span, bool) {
	if n.Type() != ast.TypeBlock {
		return Span{}, false
	}
	lines := n.Lines()
	if lines == nil || lines.Len() == 0 {
		return Span{}, false
	}
	first, last := lines.At(0), lines.At(lines.Len()-1)
	return Span{Start: first.Start, End: last.Stop}, true
}

// lineText joins a block node's source lines verbatim, preserving the newlines
// that separate them.
func (c *converter) lineText(n ast.Node) string {
	lines := n.Lines()
	if lines == nil {
		return ""
	}
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(c.src))
	}
	return b.String()
}

// inlineText concatenates the literal text of an inline node's children, used
// for code spans and image alt text where the client wants a plain string.
func (c *converter) inlineText(n ast.Node) string {
	var b strings.Builder
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		switch t := ch.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(c.src))
		case *ast.String:
			b.Write(t.Value)
		default:
			b.WriteString(c.inlineText(ch))
		}
	}
	return b.String()
}

func (c *converter) rawHTMLText(r *ast.RawHTML) string {
	var b strings.Builder
	for i := 0; i < r.Segments.Len(); i++ {
		seg := r.Segments.At(i)
		b.Write(seg.Value(c.src))
	}
	return b.String()
}

// taskState reports whether a list item is a GFM task item and, if so, whether
// it is ticked. The checkbox lives one level down, inside the item's first
// block, and is dropped from the tree once hoisted here.
func taskState(item ast.Node) (bool, bool) {
	for block := item.FirstChild(); block != nil; block = block.NextSibling() {
		for inline := block.FirstChild(); inline != nil; inline = inline.NextSibling() {
			if cb, ok := inline.(*east.TaskCheckBox); ok {
				return cb.IsChecked, true
			}
		}
	}
	return false, false
}

func isAutoLink(n ast.Node) bool {
	_, ok := n.(*ast.AutoLink)
	return ok
}

func alignName(a east.Alignment) string {
	switch a {
	case east.AlignLeft:
		return "left"
	case east.AlignRight:
		return "right"
	case east.AlignCenter:
		return "center"
	default:
		return ""
	}
}
