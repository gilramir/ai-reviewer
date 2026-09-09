package mdast

import (
	"encoding/json"
	"strings"
	"testing"
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
		if n.Span.Start < 0 || n.Span.End > len(src) || n.Span.Start > n.Span.End {
			t.Fatalf("%s %s: span %v out of bounds (len %d)", n.Kind, n.ID, n.Span, len(src))
		}
		if n.Kind == KindText {
			got := string(src[n.Span.Start:n.Span.End])
			if got != n.Text {
				t.Errorf("%s: span yields %q, node text is %q", n.ID, got, n.Text)
			}
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
			if c.Span.Start < n.Span.Start || c.Span.End > n.Span.End {
				t.Errorf("%s %s span %v escapes parent %s %s span %v",
					c.Kind, c.ID, c.Span, n.Kind, n.ID, n.Span)
			}
			check(c)
		}
	}
	check(doc.Root)
}

func TestHeading(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	hs := find(doc.Root, KindHeading)
	if len(hs) != 1 {
		t.Fatalf("want 1 heading, got %d", len(hs))
	}
	if hs[0].Level != 1 {
		t.Errorf("want level 1, got %d", hs[0].Level)
	}
	if got := string([]byte(sample)[hs[0].Span.Start:hs[0].Span.End]); got != "Retry policy" {
		t.Errorf("heading span yields %q", got)
	}
}

func TestCodeBlockKeepsLanguageAndText(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	cbs := find(doc.Root, KindCodeBlock)
	if len(cbs) != 1 {
		t.Fatalf("want 1 code block, got %d", len(cbs))
	}
	if cbs[0].Lang != "go" {
		t.Errorf("want lang go, got %q", cbs[0].Lang)
	}
	if cbs[0].Text != "func main() {}\n" {
		t.Errorf("code text = %q", cbs[0].Text)
	}
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
	if checked != 1 || unchecked != 1 {
		t.Errorf("want 1 checked and 1 unchecked task item, got %d/%d", checked, unchecked)
	}
	// The checkbox itself must not survive into the tree.
	walk(doc.Root, func(n Node) {
		if n.Kind == KindText && strings.HasPrefix(n.Text, "[") && len(n.Text) == 3 {
			t.Errorf("checkbox leaked into tree as text node %q", n.Text)
		}
	})
}

func TestTableAlignmentAndHeader(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	rows := find(doc.Root, KindTableRow)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (header + body), got %d", len(rows))
	}
	if !rows[0].Header {
		t.Error("first row should be the header")
	}
	cells := find(doc.Root, KindTableCell)
	if len(cells) != 4 {
		t.Fatalf("want 4 cells, got %d", len(cells))
	}
	if cells[1].Align != "right" {
		t.Errorf("second column should be right-aligned, got %q", cells[1].Align)
	}
	if !cells[0].Header || cells[2].Header {
		t.Error("header flag should distinguish header cells from body cells")
	}
}

func TestEmphasisAndStrong(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	if n := len(find(doc.Root, KindEmphasis)); n != 1 {
		t.Errorf("want 1 emphasis, got %d", n)
	}
	if n := len(find(doc.Root, KindStrong)); n != 1 {
		t.Errorf("want 1 strong, got %d", n)
	}
	spans := find(doc.Root, KindCodeSpan)
	if len(spans) != 1 || spans[0].Text != "item" {
		t.Errorf("code span = %+v", spans)
	}
}

func TestDocumentSpansWholeSource(t *testing.T) {
	doc := Render("sample.md", 7, []byte(sample))
	if doc.Root.Span.Start != 0 || doc.Root.Span.End != len(sample) {
		t.Errorf("document span %v, want 0..%d", doc.Root.Span, len(sample))
	}
	if doc.Rev != 7 || doc.Path != "sample.md" {
		t.Errorf("metadata not carried: %+v", doc)
	}
}

// TestIDsAreUnique matters because the client keys its virtual DOM on them.
func TestIDsAreUnique(t *testing.T) {
	doc := Render("sample.md", 1, []byte(sample))
	seen := map[string]bool{}
	walk(doc.Root, func(n Node) {
		if seen[n.ID] {
			t.Errorf("duplicate node id %q", n.ID)
		}
		seen[n.ID] = true
	})
}

func TestJSONShapeIsStable(t *testing.T) {
	doc := Render("sample.md", 1, []byte("hello *there*\n"))
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// omitempty must keep absent fields out so the Gren decoder can rely on
	// optional fields genuinely being absent rather than zero-valued.
	if strings.Contains(string(b), `"level"`) || strings.Contains(string(b), `"lang"`) {
		t.Errorf("empty optional fields leaked into JSON: %s", b)
	}
}

func TestEmptyDocument(t *testing.T) {
	doc := Render("empty.md", 1, nil)
	if doc.Root.Kind != KindDocument {
		t.Errorf("want document root, got %q", doc.Root.Kind)
	}
	if len(doc.Root.Children) != 0 {
		t.Errorf("want no children, got %d", len(doc.Root.Children))
	}
}
