package wirelog

import (
	"encoding/json"
	"fmt"
	"strings"
)

// frameInfo is what the log takes out of a frame on the way in: the columns the
// list is drawn from, and the links between frames. The frame itself is stored
// whole beside it.
type frameInfo struct {
	typ             string
	subtype         string
	messageID       string
	parentToolUseID string
	model           string
	blocks          []block
	uses            []toolUse
	results         []toolResult
	result          *resultFields
	summary         string
}

type toolUse struct{ id, name string }

type toolResult struct {
	id      string
	isError bool
}

// wireFrame is the subset of every frame shape the log reads. Fields absent
// from a given frame decode as zero values, which is what lets one struct serve
// init, assistant, user and result alike -- and an unknown type, which yields
// its type name and nothing else.
type wireFrame struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	Model           string          `json:"model"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Message         json.RawMessage `json:"message"`
	Result          *string         `json:"result"`
	resultFields
}

type resultFields struct {
	IsError       bool    `json:"is_error"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	DurationMS    int64   `json:"duration_ms"`
	DurationAPIMS int64   `json:"duration_api_ms"`
	NumTurns      int64   `json:"num_turns"`
	Usage         struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type wireMessage struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// parseFrame reads what it can from one frame and never fails: a frame that
// is not the shape expected is still logged, with fewer columns filled in.
func parseFrame(line []byte) frameInfo {
	var w wireFrame
	if json.Unmarshal(line, &w) != nil {
		return frameInfo{}
	}

	f := frameInfo{
		typ:             w.Type,
		subtype:         w.Subtype,
		model:           w.Model,
		parentToolUseID: w.ParentToolUseID,
	}
	if w.Type == "result" {
		r := w.resultFields
		f.result = &r
	}

	var m wireMessage
	if len(w.Message) > 0 && json.Unmarshal(w.Message, &m) == nil {
		f.messageID = m.ID
		// A user prompt may carry its content as a bare string rather than
		// an array of blocks.
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			f.blocks = []block{{Type: "text", Text: text}}
		} else {
			_ = json.Unmarshal(m.Content, &f.blocks)
		}
	}

	for _, b := range f.blocks {
		switch b.Type {
		case "tool_use":
			f.uses = append(f.uses, toolUse{id: b.ID, name: b.Name})
		case "tool_result":
			f.results = append(f.results, toolResult{id: b.ToolUseID, isError: b.IsError})
		}
	}
	if w.Type == "result" && w.Result != nil {
		f.blocks = []block{{Type: "text", Text: *w.Result}}
	}
	return f
}

// summariseIn is the one-line description of a frame written to stdin.
func summariseIn(f frameInfo) string {
	return "prompt: " + firstLine(blockText(f.blocks), 160)
}

// summarise is the one-line description of a stdout frame, which is what the
// list shows. tools names the calls still awaiting a result, so a tool_result
// can say which tool it came from.
func summarise(f frameInfo, tools map[string]string) string {
	switch f.typ {
	case "system":
		if f.subtype == "init" {
			return "init " + f.model
		}
		return "system " + f.subtype

	case "assistant":
		parts := make([]string, 0, len(f.blocks))
		for _, b := range f.blocks {
			switch b.Type {
			case "text":
				parts = append(parts, quoted(firstLine(b.Text, 120)))
			case "thinking":
				parts = append(parts, "thinking")
			case "tool_use":
				parts = append(parts, b.Name+" "+toolTarget(b.Input))
			default:
				parts = append(parts, b.Type)
			}
		}
		return strings.Join(parts, "; ")

	case "user":
		parts := make([]string, 0, len(f.blocks))
		for _, b := range f.blocks {
			if b.Type != "tool_result" {
				parts = append(parts, b.Type)
				continue
			}
			name := tools[b.ToolUseID]
			if name == "" {
				name = "tool"
			}
			prefix := name + " result: "
			if b.IsError {
				prefix = name + " error: "
			}
			parts = append(parts, prefix+firstLine(resultText(b.Content), 100))
		}
		return strings.Join(parts, "; ")

	case "result":
		s := f.subtype
		if f.result != nil {
			s += fmt.Sprintf(" $%.4f %.1fs", f.result.TotalCostUSD, float64(f.result.DurationMS)/1000)
		}
		if text := blockText(f.blocks); text != "" {
			s += " " + quoted(firstLine(text, 100))
		}
		return s
	}

	if f.subtype != "" {
		return f.typ + " " + f.subtype
	}
	return f.typ
}

// toolTarget is the argument that says what a tool call is about: the file for
// the file tools, the pattern for the search ones.
func toolTarget(input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
		Pattern  string `json:"pattern"`
		Path     string `json:"path"`
	}
	_ = json.Unmarshal(input, &in)
	switch {
	case in.FilePath != "":
		return in.FilePath
	case in.Pattern != "" && in.Path != "":
		return in.Pattern + " in " + in.Path
	case in.Pattern != "":
		return in.Pattern
	}
	return in.Path
}

// resultText flattens a tool_result's content, which is either a string or an
// array of text blocks.
func resultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []block
	_ = json.Unmarshal(content, &blocks)
	return blockText(blocks)
}

func blockText(blocks []block) string {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String()
}

// quoted marks prose off from the words around it in a summary. Not %q: the
// list is for reading, and prose about code is full of quotes that would come
// out as a thicket of backslashes.
func quoted(s string) string {
	return "“" + s + "”"
}

// firstLine is the start of s up to its first non-blank line break, cut to
// at most n runes.
func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i]) + " …"
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
