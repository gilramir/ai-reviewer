package mdast

// Sections divides a document into the stretches a reviewing pass works
// through, one turn each.
//
// The unit is a heading section because that is the unit an author wrote in,
// and because the alternative -- the whole document in one prompt -- gives
// about the same handful of comments whether the document is three hundred
// words or five thousand. A section also keeps the text a quote has to be found
// in small, which matters when the text is searched for a phrase the model
// chose rather than one a reader selected.
//
// Sections are ranges into Rendered.Text rather than subtrees, so the string a
// model is asked to quote out of is a literal substring of the string Locate
// searches. Any other arrangement puts a second opinion between the two.
type Section struct {
	// Title is the heading that opens the section, or empty for the run of
	// content before the first one.
	Title string
	Level int

	Start int
	End   int
}

// Slice is the section as the reader sees it. Named for the operation rather
// than the thing because Rendered.Text is already taken by the whole document,
// and the two being confusable is the bug this package spends its comments on.
func (r Rendered) Slice(s Section) string { return r.Text[s.Start:s.End] }

// Sections splits a document, keeping each piece under maxBytes where the block
// structure allows it. A maxBytes of zero applies no cap.
func (r Rendered) Sections(maxBytes int) []Section {
	if len(r.Blocks) == 0 {
		if r.Text == "" {
			return nil
		}
		return []Section{{Start: 0, End: len(r.Text)}}
	}

	level := splitLevel(r.Blocks)

	var out []Section
	current := Section{Start: r.Blocks[0].TextStart, End: r.Blocks[0].TextStart}

	for _, block := range r.Blocks {
		opens := level > 0 && block.Kind == KindHeading && block.Level <= level
		if opens && current.End > current.Start {
			out = append(out, current)
			current = Section{Start: block.TextStart, End: block.TextStart}
		}
		if opens && current.Title == "" {
			current.Title, current.Level = block.Title, block.Level
		}
		current.End = block.TextEnd
	}
	if current.End > current.Start {
		out = append(out, current)
	}

	return capped(out, r.Blocks, maxBytes)
}

// splitLevel picks the heading level a document divides at: the shallowest one
// that occurs more than once.
//
// Not simply the shallowest, because a document with one title and eight `##`
// headings under it has exactly one level-1 heading, and splitting there
// returns the whole file as a single section -- which is the case this exists
// to avoid. A level that occurs once is a title; a level that occurs repeatedly
// is a structure. Zero means the document has no headings to divide at.
func splitLevel(blocks []Block) int {
	counts := map[int]int{}
	for _, block := range blocks {
		if block.Kind == KindHeading && block.Level > 0 {
			counts[block.Level]++
		}
	}
	if len(counts) == 0 {
		return 0
	}

	repeated, only := 0, 0
	for level, n := range counts {
		if only == 0 || level < only {
			only = level
		}
		if n > 1 && (repeated == 0 || level < repeated) {
			repeated = level
		}
	}
	if repeated > 0 {
		return repeated
	}
	return only
}

// capped splits any section longer than maxBytes on the block boundaries inside
// it. A single block over the cap is left whole: there is no boundary inside it
// to cut on, and cutting anywhere else would hand the model a fragment of a
// sentence to quote out of.
func capped(sections []Section, blocks []Block, maxBytes int) []Section {
	if maxBytes <= 0 {
		return sections
	}

	var out []Section
	for _, section := range sections {
		if section.End-section.Start <= maxBytes {
			out = append(out, section)
			continue
		}

		piece := Section{Title: section.Title, Level: section.Level, Start: section.Start, End: section.Start}
		for _, block := range blocks {
			if block.TextStart < section.Start || block.TextEnd > section.End {
				continue
			}
			if piece.End > piece.Start && block.TextEnd-piece.Start > maxBytes {
				out = append(out, piece)
				// Continuations keep the title: they are still that section,
				// and the reviewer reading "3 of 9" wants to know where they
				// are, not which slice they are in.
				piece = Section{Title: section.Title, Level: section.Level, Start: block.TextStart, End: block.TextStart}
			}
			piece.End = block.TextEnd
		}
		if piece.End > piece.Start {
			out = append(out, piece)
		}
	}
	return out
}
