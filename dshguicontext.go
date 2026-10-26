package main

import (
	"sort"
	"strings"
)

const (
	dshGUIContextSentence = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUIContextNeutral  = "You are interacting with the user through a Web GUI whose URL the runtime context names."
	// Match the known opening while preserving all surrounding instructions.
	dshGUIContextOpening = dshGUIContextSentence + " When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."
)

type dshTextRange struct{ start, end int }

// Keep the original container so edits retain content arrays and block metadata.
type dshMessageTextPart struct {
	container map[string]any
	key, text string
	start     int
}

func dshMessageText(message map[string]any, anthropic bool) (string, []dshMessageTextPart) {
	parts := dshMessageTextParts(message, anthropic)
	return joinDSHMessageText(parts), parts
}

// Collect borrowed text and its offsets before allocating a Markdown parsing
// view. Unmatched messages need no joined copy; matching messages still use all
// parts, including earlier parts that open code spans or fences.
func dshMessageTextParts(message map[string]any, anthropic bool) []dshMessageTextPart {
	if text, ok := message["content"].(string); ok {
		return []dshMessageTextPart{{container: message, key: "content", text: text}}
	}
	blocks, _ := message["content"].([]any)
	separatorBytes := 1
	if anthropic && roleOfMessage(message) == "assistant" {
		// convertAnthropicBlocks joins assistant text verbatim; system adds newlines.
		separatorBytes = 0
	}
	length := 0
	var parts []dshMessageTextPart
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] != "text" {
			continue
		}
		text, ok := block["text"].(string)
		if !ok || text == "" {
			continue
		}
		if len(parts) > 0 {
			length += separatorBytes
		}
		parts = append(parts, dshMessageTextPart{container: block, key: "text", text: text, start: length})
		length += len(text)
	}
	return parts
}

func joinDSHMessageText(parts []dshMessageTextPart) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0].text
	}
	var joined strings.Builder
	last := parts[len(parts)-1]
	joined.Grow(last.start + len(last.text))
	for _, part := range parts {
		if joined.Len() < part.start {
			joined.WriteByte('\n')
		}
		joined.WriteString(part.text)
	}
	return joined.String()
}

func dshFenceDelimiter(line string) (byte, int, string) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return 0, 0, ""
	}
	n := 1
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	return trimmed[0], n, trimmed[n:]
}

// Index matching runs once. Unmatched backticks are literal Markdown, and inline
// code cannot cross a blank paragraph. Backslashes do not escape closing runs.
func dshCodeSpanEnds(text string) map[int]int {
	type tickRun struct{ start, length, paragraph int }
	var runs []tickRun
	paragraph := 0
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		line := strings.TrimSuffix(text[start:end], "\r")
		marker, _, info := dshFenceDelimiter(line)
		fenceLine := marker != 0 && (marker != '`' || !strings.ContainsRune(info, '`'))
		if strings.Trim(line, " \t") == "" || fenceLine {
			paragraph++
		}
		// Fences interrupt paragraphs before inline Markdown is parsed, so a
		// backtick before the fence cannot pair with a run in or after it.
		if fenceLine {
			start = end + 1
			continue
		}
		for pos := start; pos < end; {
			if text[pos] != '`' {
				pos++
				continue
			}
			n := 1
			for pos+n < end && text[pos+n] == '`' {
				n++
			}
			runs = append(runs, tickRun{pos, n, paragraph})
			pos += n
		}
		start = end + 1
	}
	ends := make(map[int]int)
	next := make(map[int]int)
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if run.paragraph != paragraph {
			clear(next)
			paragraph = run.paragraph
		}
		if close, ok := next[run.length]; ok {
			ends[run.start] = close + run.length
		}
		// An escaped first backtick leaves a possible opening run of length n-1.
		if run.length > 1 {
			if close, ok := next[run.length-1]; ok {
				ends[run.start+1] = close + run.length - 1
			}
		}
		next[run.length] = run.start
	}
	return ends
}

func dshMarkdownCodeRanges(text string) []dshTextRange {
	spanEnds := dshCodeSpanEnds(text)
	var ranges []dshTextRange
	add := func(start, end int) {
		if n := len(ranges); n > 0 && start <= ranges[n-1].end {
			ranges[n-1].end = max(end, ranges[n-1].end)
		} else {
			ranges = append(ranges, dshTextRange{start, end})
		}
	}
	var fence byte
	fenceLength, inlineEnd := 0, 0
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text)
		} else {
			end += start
		}
		line := strings.TrimSuffix(text[start:end], "\r")
		next := min(end+1, len(text))
		trimmed := strings.TrimLeft(line, " ")
		isFence := false
		if inlineEnd <= start {
			marker, n, info := dshFenceDelimiter(line)
			if marker != 0 {
				if fence == 0 && (marker != '`' || !strings.ContainsRune(info, '`')) {
					fence, fenceLength, isFence = marker, n, true
				} else if fence == marker && n >= fenceLength && strings.Trim(info, " \t") == "" {
					fence, fenceLength, isFence = 0, 0, true
				}
			}
		}
		indentedCode := len(line)-len(trimmed) >= 4 || strings.HasPrefix(line, "\t")
		if isFence || fence != 0 || (indentedCode && inlineEnd <= start) {
			add(start, next)
		} else {
			for pos := max(start, inlineEnd); pos < end; {
				if text[pos] == '\\' && pos+1 < end && (text[pos+1] == '\\' || text[pos+1] == '`') {
					pos += 2
					continue
				}
				if text[pos] != '`' {
					pos++
					continue
				}
				n := 1
				for pos+n < end && text[pos+n] == '`' {
					n++
				}
				if close, ok := spanEnds[pos]; ok {
					add(pos, close)
					inlineEnd, pos = close, close
				} else {
					pos += n
				}
			}
		}
		start = next
	}
	return ranges
}

func dshPositionInCode(ranges []dshTextRange, pos int) bool {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].end > pos })
	return i < len(ranges) && ranges[i].start <= pos
}

// Match within original parts; the joined text is only a Markdown parsing view.
func normalizeDSHGUIContextPart(text string, start int, code []dshTextRange) (string, int) {
	var out strings.Builder
	copied, changed := 0, 0
	for pos := 0; pos < len(text); {
		match := strings.Index(text[pos:], dshGUIContextOpening)
		if match < 0 {
			break
		}
		match += pos
		if !dshPositionInCode(code, start+match) {
			out.WriteString(text[copied:match])
			out.WriteString(dshGUIContextNeutral)
			copied = match + len(dshGUIContextSentence)
			changed++
		}
		pos = match + len(dshGUIContextOpening)
	}
	if changed == 0 {
		return text, 0
	}
	out.WriteString(text[copied:])
	return out.String(), changed
}

// Change only the known environment sentence outside code, retaining all other bytes.
func normalizeDSHGUIContextText(text string) (string, int) {
	if !strings.Contains(text, dshGUIContextOpening) {
		return text, 0
	}
	return normalizeDSHGUIContextPart(text, 0, dshMarkdownCodeRanges(text))
}

func normalizeDSHGUIContexts(obj map[string]any) int {
	return normalizeDSHGUIContextsForProtocol(obj, false)
}

func normalizeDSHGUIContextsForProtocol(obj map[string]any, anthropic bool) int {
	messages, _ := obj["messages"].([]any)
	changed := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || (roleOfMessage(message) != "system" && roleOfMessage(message) != "assistant") {
			continue
		}
		parts := dshMessageTextParts(message, anthropic)
		candidate := false
		for _, part := range parts {
			// Replacement already requires a complete opening inside one part.
			if strings.Contains(part.text, dshGUIContextOpening) {
				candidate = true
				break
			}
		}
		if !candidate {
			continue
		}
		code := dshMarkdownCodeRanges(joinDSHMessageText(parts))
		for _, part := range parts {
			if normalized, n := normalizeDSHGUIContextPart(part.text, part.start, code); n > 0 {
				part.container[part.key] = normalized
				changed += n
			}
		}
	}
	return changed
}
