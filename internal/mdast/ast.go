// Package mdast converts Markdown source into a JSON-serialisable AST whose
// nodes carry byte spans back into the original file.
//
// The spans are the whole point: the browser never re-parses Markdown, so the
// positions the front-end reasons about are the ones goldmark actually derived.
// Keeping a single parser in the system means an anchor can never drift because
// two implementations disagreed about where a paragraph starts.
package mdast

// Span is a half-open byte range [Start, End) into the document source.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Kind enumerates every node type the front-end knows how to render. Adding a
// kind here without teaching the Gren decoder about it is a decode error rather
// than a silently blank paragraph, which is the trade we want.
type Kind string

const (
	KindDocument      Kind = "document"
	KindParagraph     Kind = "paragraph"
	KindHeading       Kind = "heading"
	KindCodeBlock     Kind = "codeBlock"
	KindBlockQuote    Kind = "blockQuote"
	KindList          Kind = "list"
	KindListItem      Kind = "listItem"
	KindThematicBreak Kind = "thematicBreak"
	KindHTMLBlock     Kind = "htmlBlock"
	KindTable         Kind = "table"
	KindTableRow      Kind = "tableRow"
	KindTableCell     Kind = "tableCell"

	KindText          Kind = "text"
	KindEmphasis      Kind = "emphasis"
	KindStrong        Kind = "strong"
	KindStrikethrough Kind = "strikethrough"
	KindCodeSpan      Kind = "codeSpan"
	KindLink          Kind = "link"
	KindImage         Kind = "image"
	KindLineBreak     Kind = "lineBreak"
	KindRawHTML       Kind = "rawHtml"
)

// Node is a single element of the document tree. One flat struct covers both
// block and inline nodes: the recursion is uniform, which keeps the Gren
// decoder to a single lazy union rather than two mutually recursive ones.
type Node struct {
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
	Span Span   `json:"span"`

	// Line is the 1-based source line the node starts on, derived from
	// Span.Start. The reviewer sees these in the margin, so a model that cites
	// "line 125" is talking about a place they can find. Zero for the nodes
	// that have no span of their own.
	Line int `json:"line,omitempty"`

	// Text carries the literal content of leaf nodes (text, codeSpan,
	// codeBlock, rawHtml). Empty for everything else.
	Text string `json:"text,omitempty"`

	Level     int    `json:"level,omitempty"`     // heading
	Lang      string `json:"lang,omitempty"`      // codeBlock
	Ordered   bool   `json:"ordered,omitempty"`   // list
	ListStart int    `json:"listStart,omitempty"` // list, when it starts at != 1
	Checked   *bool  `json:"checked,omitempty"`   // listItem, GFM task lists
	URL       string `json:"url,omitempty"`       // link, image
	Title     string `json:"title,omitempty"`     // link, image
	Alt       string `json:"alt,omitempty"`       // image
	Header    bool   `json:"header,omitempty"`    // tableRow, tableCell
	Align     string `json:"align,omitempty"`     // tableCell: left|right|center

	Children []Node `json:"children,omitempty"`
}

// Document is the payload pushed to the browser on every render.
type Document struct {
	Path string `json:"path"`
	// Rev increments on every successful render of this path, so the client can
	// discard an out-of-order frame without comparing trees.
	Rev  int  `json:"rev"`
	Root Node `json:"root"`
}
