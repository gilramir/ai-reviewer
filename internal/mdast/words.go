package mdast

import "unicode"

// Token is one unit of a document's text: a run of word characters, or the
// whitespace separating two of them.
//
// Start and End are code point offsets into the text the browser concatenates
// out of this tree -- the coordinates `Doc.text` counts in, and the ones a
// highlight range is given in -- so a token located here can be pointed at on
// the page without any further translation.
type Token struct {
	Text  string
	Start int
	End   int
	Space bool
}

// Words splits a document into the units a change is measured in.
//
// Splitting each run of text on its own, rather than the concatenation of all
// of them, is what keeps a word inside the element it belongs to: blocks are
// joined with nothing between them, so the last word of one paragraph and the
// first of the next sit against each other in that string and a tokeniser
// reading it would make them one word.
//
// It also makes re-wrapping free. A soft wrap reaches the tree as a line break
// node worth one space, so `foo\nbar` and `foo bar` arrive here as the same
// three tokens, and a paragraph the model re-flowed without changing a word is
// reported as unchanged.
func Words(root Node) []Token {
	var w words
	w.walk(root)
	return w.tokens
}

type words struct {
	tokens []Token
	at     int // code points emitted so far
}

// walk visits the tree in the order the text is concatenated in: a node's own
// contribution, then its children's. This is the order `Doc.text` uses, and the
// offsets below are only meaningful while it stays that way.
func (w *words) walk(node Node) {
	w.emit(ownText(node))
	for _, child := range node.Children {
		w.walk(child)
	}
}

func (w *words) emit(text string) {
	runes := []rune(text)
	for i := 0; i < len(runes); {
		space := unicode.IsSpace(runes[i])
		j := i + 1
		for j < len(runes) && unicode.IsSpace(runes[j]) == space {
			j++
		}
		w.tokens = append(w.tokens, Token{
			Text:  string(runes[i:j]),
			Start: w.at + i,
			End:   w.at + j,
			Space: space,
		})
		i = j
	}
	w.at += len(runes)
}
