package mdast

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sample = "# Retry policy\n" +
	"\n" +
	"The system SHALL retry indefinitely until the operation succeeds.\n" +
	"\n" +
	"- first *item*\n" +
	"- second `item`\n" +
	"\n" +
	"> A quoted **caution**.\n" +
	"\n" +
	"```go\n" +
	"func main() {}\n" +
	"```\n" +
	"\n" +
	"| a | b |\n" +
	"|---|--:|\n" +
	"| 1 | 2 |\n" +
	"\n" +
	"- [x] done\n" +
	"- [ ] todo\n"

// walk visits every node depth-first.
func walk(n Node, fn func(Node)) {
	fn(n)
	for _, c := range n.Children {
		walk(c, fn)
	}
}

func find(root Node, kind Kind) []Node {
	var out []Node
	walk(root, func(n Node) {
		if n.Kind == kind {
			out = append(out, n)
		}
	})
	return out
}

// TestSpansSliceBackToSource is the load-bearing test: every non-zero span must
// index the original bytes, and a text node's span must recover exactly its own
// text. If this breaks, comment anchors land on the wrong paragraph.
func TestSpansSliceBackToSource(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	src := []byte(sample)

	walk(doc.Root, func(n Node) {
		if n.Span == (Span{}) {
			return
		}
		require.True(t, n.Span.Start >= 0 && n.Span.End <= len(src) && n.Span.Start <= n.Span.End,
			"%s %s: span %v out of bounds (len %d)", n.Kind, n.ID, n.Span, len(src))
		if n.Kind == KindText {
			assert.Equal(t, n.Text, string(src[n.Span.Start:n.Span.End]), "%s: what the span yields", n.ID)
		}
	})
}

func TestSpansNestWithinParents(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))

	var check func(Node)
	check = func(n Node) {
		for _, c := range n.Children {
			if c.Span == (Span{}) || n.Span == (Span{}) {
				continue
			}
			assert.True(t, c.Span.Start >= n.Span.Start && c.Span.End <= n.Span.End,
				"%s %s span %v escapes parent %s %s span %v",
				c.Kind, c.ID, c.Span, n.Kind, n.ID, n.Span)
			check(c)
		}
	}
	check(doc.Root)
}

func TestHeading(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	hs := find(doc.Root, KindHeading)
	require.Len(t, hs, 1)
	assert.Equal(t, 1, hs[0].Level)
	assert.Equal(t, "Retry policy", string([]byte(sample)[hs[0].Span.Start:hs[0].Span.End]), "what the heading span yields")
}

func TestCodeBlockKeepsLanguageAndText(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	cbs := find(doc.Root, KindCodeBlock)
	require.Len(t, cbs, 1)
	assert.Equal(t, "go", cbs[0].Lang)
	assert.Equal(t, "func main() {}\n", cbs[0].Text)
}

func TestTaskListHoistedOntoItem(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	var checked, unchecked int
	walk(doc.Root, func(n Node) {
		if n.Kind == KindListItem && n.Checked != nil {
			if *n.Checked {
				checked++
			} else {
				unchecked++
			}
		}
	})
	assert.Equal(t, 1, checked, "checked task items")
	assert.Equal(t, 1, unchecked, "unchecked task items")
	// The checkbox itself must not survive into the tree.
	walk(doc.Root, func(n Node) {
		if n.Kind == KindText {
			assert.False(t, strings.HasPrefix(n.Text, "[") && len(n.Text) == 3,
				"checkbox leaked into tree as text node %q", n.Text)
		}
	})
}

func TestTableAlignmentAndHeader(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	rows := find(doc.Root, KindTableRow)
	require.Len(t, rows, 2, "header + body")
	assert.True(t, rows[0].Header, "first row should be the header")
	cells := find(doc.Root, KindTableCell)
	require.Len(t, cells, 4)
	assert.Equal(t, "right", cells[1].Align, "second column should be right-aligned")
	assert.True(t, cells[0].Header, "header flag should distinguish header cells from body cells")
	assert.False(t, cells[2].Header, "header flag should distinguish header cells from body cells")
}

func TestEmphasisAndStrong(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	assert.Len(t, find(doc.Root, KindEmphasis), 1)
	assert.Len(t, find(doc.Root, KindStrong), 1)
	spans := find(doc.Root, KindCodeSpan)
	if assert.Len(t, spans, 1) {
		assert.Equal(t, "item", spans[0].Text)
	}
}

func TestDocumentSpansWholeSource(t *testing.T) {
	doc := Render("sample.md", 7, []byte(sample))
	assert.Equal(t, Span{Start: 0, End: len(sample)}, doc.Root.Span)
	assert.Equal(t, 7, doc.Rev, "metadata not carried")
	assert.Equal(t, "sample.md", doc.Path, "metadata not carried")
}

// TestIDsAreUnique matters because the client keys its virtual DOM on them.
func TestIDsAreUnique(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	seen := map[string]bool{}
	walk(doc.Root, func(n Node) {
		assert.False(t, seen[n.ID], "duplicate node id %q", n.ID)
		seen[n.ID] = true
	})
}

func TestJSONShapeIsStable(t *testing.T) {
	doc := Render("sample.md", 1, []byte("hello *there*\n"))
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	// omitempty must keep absent fields out so the Gren decoder can rely on
	// optional fields genuinely being absent rather than zero-valued.
	assert.NotContains(t, string(b), `"level"`, "empty optional fields leaked into JSON")
	assert.NotContains(t, string(b), `"lang"`, "empty optional fields leaked into JSON")
}

func TestEmptyDocument(t *testing.T) {
	doc := Render("empty.md", 1, nil)
	assert.Equal(t, KindDocument, doc.Root.Kind)
	assert.Empty(t, doc.Root.Children)
}

// sourceLine returns the 1-based line n of src.
func sourceLine(src string, n int) string {
	lines := strings.Split(src, "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// TestLinesMatchSource is the counterpart to the span test for the margin the
// reviewer reads: a node's Line must be the line its span actually starts on,
// because the model cites line numbers and the reviewer has to find them.
func TestLinesMatchSource(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	src := []byte(sample)

	walk(doc.Root, func(n Node) {
		if n.Line == 0 {
			assert.Equal(t, Span{}, n.Span, "%s %s: a span but no line", n.Kind, n.ID)
			return
		}
		want := 1 + strings.Count(string(src[:n.Span.Start]), "\n")
		if n.Kind == KindCodeBlock && strings.HasPrefix(sourceLine(sample, want-1), "```") {
			// A fence is reported at the fence, not at the first line of code.
			want--
		}
		assert.Equal(t, want, n.Line, "%s %s", n.Kind, n.ID)
	})
}

func TestLineOfEachTopLevelBlock(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	want := []int{1, 3, 5, 8, 10, 14, 18}
	require.Len(t, doc.Root.Children, len(want), "top-level blocks")
	for i, block := range doc.Root.Children {
		assert.Equal(t, want[i], block.Line, "block %d (%s)", i, block.Kind)
	}
}
