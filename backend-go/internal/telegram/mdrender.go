// Package telegram — mdrender converts LLM-produced markdown to the Telegram
// HTML subset or to stripped plain text. Used when sendRichMessage is
// unavailable: the caller degrades through HTML → plain rather than dumping
// raw markdown tokens (### ** ~~) into the chat.
package telegram

import (
	"strings"
	"unicode"
)

// MarkdownToHTML converts a markdown string to the Telegram HTML subset
// (parse_mode=HTML). Supported constructs:
//
//   - Fenced code blocks (```lang … ```) → <pre>…</pre>
//   - Inline code (`…`) → <code>…</code>
//   - Headings (#–######) → bold line
//   - Blockquotes (> …) → <blockquote>…</blockquote>
//   - Unordered lists (- / * / +) → • item
//   - Ordered lists (1. …) → N. item
//   - Table rows (|…|) → space-joined cell text
//   - **bold** / __bold__ → <b>…</b>
//   - *italic* / _italic_ → <i>…</i>
//   - ~~strike~~ → <s>…</s>
//   - [text](url) → <a href="url">text</a>
//
// Literal text is HTML-escaped (&→&amp; <→&lt; >→&gt;).
// Table header-separator rows (|---|) are silently dropped.
func MarkdownToHTML(md string) string {
	lines := strings.Split(md, "\n")
	var out strings.Builder
	out.Grow(len(md) * 2)

	inFence := false
	var fenceLang string
	var fenceLines []string

	flushFence := func() {
		code := htmlEscape(strings.Join(fenceLines, "\n"))
		if fenceLang != "" {
			out.WriteString("<pre><code>")
			out.WriteString(code)
			out.WriteString("</code></pre>")
		} else {
			out.WriteString("<pre>")
			out.WriteString(code)
			out.WriteString("</pre>")
		}
		fenceLines = nil
		fenceLang = ""
	}

	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			if inFence {
				flushFence()
				inFence = false
				appendNewline(&out, i, len(lines))
			} else {
				inFence = true
				fenceLang = strings.TrimSpace(strings.TrimPrefix(line, "```"))
			}
			continue
		}
		if inFence {
			fenceLines = append(fenceLines, line)
			continue
		}

		rendered := renderBlockLine(line)
		out.WriteString(rendered)
		appendNewline(&out, i, len(lines))
	}
	if inFence {
		flushFence()
	}

	return out.String()
}

// StripMarkdown removes markdown syntax, producing readable plain text.
// Heading `#` prefixes are dropped; list bullets become `• `; inline
// markers (**/_/~~/ ` ) are removed. Triple newlines are collapsed to two.
func StripMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var out strings.Builder
	out.Grow(len(md))

	inFence := false

	for i, line := range lines {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			appendNewline(&out, i, len(lines))
			continue
		}
		if inFence {
			out.WriteString(line)
			appendNewline(&out, i, len(lines))
			continue
		}

		// Headings — drop the # prefix
		stripped := stripHeadingPrefix(line)
		if stripped != line {
			out.WriteString(stripInline(stripped))
			appendNewline(&out, i, len(lines))
			continue
		}

		// Unordered lists
		if item, ok := unorderedListItem(line); ok {
			out.WriteString("• ")
			out.WriteString(stripInline(item))
			appendNewline(&out, i, len(lines))
			continue
		}

		// Ordered lists — keep the number
		if item, n, ok := orderedListItem(line); ok {
			out.WriteString(itoa(n))
			out.WriteString(". ")
			out.WriteString(stripInline(item))
			appendNewline(&out, i, len(lines))
			continue
		}

		// Table separator rows
		if isTableSeparator(line) {
			continue
		}

		// Table data rows
		if isTableRow(line) {
			out.WriteString(tableRowText(line))
			appendNewline(&out, i, len(lines))
			continue
		}

		out.WriteString(stripInline(line))
		appendNewline(&out, i, len(lines))
	}

	// Collapse triple+ newlines to double.
	result := out.String()
	for strings.Contains(result, "\n\n\n") {
		result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(result)
}

// renderBlockLine converts a single non-fence markdown line to Telegram HTML.
func renderBlockLine(line string) string {
	// Headings
	if text := stripHeadingPrefix(line); text != line {
		return "<b>" + inlineToHTML(text) + "</b>"
	}

	// Blockquote
	if strings.HasPrefix(line, "> ") {
		inner := inlineToHTML(strings.TrimPrefix(line, "> "))
		return "<blockquote>" + inner + "</blockquote>"
	}
	if line == ">" {
		return "<blockquote></blockquote>"
	}

	// Unordered list
	if item, ok := unorderedListItem(line); ok {
		return "• " + inlineToHTML(item)
	}

	// Ordered list
	if item, n, ok := orderedListItem(line); ok {
		return itoa(n) + ". " + inlineToHTML(item)
	}

	// Table separator rows
	if isTableSeparator(line) {
		return ""
	}

	// Table data rows
	if isTableRow(line) {
		return tableRowText(line)
	}

	return inlineToHTML(line)
}

// inlineToHTML converts inline markdown to Telegram HTML with escaped literal text.
// It scans left-to-right, escaping literal segments and wrapping matched spans.
func inlineToHTML(text string) string {
	var out strings.Builder
	out.Grow(len(text) * 2)
	scanInline(text, &out)
	return out.String()
}

// scanInline is the recursive inline scanner used by inlineToHTML.
func scanInline(text string, out *strings.Builder) {
	for len(text) > 0 {
		// Inline code: highest priority (not further processed inside)
		if idx := strings.Index(text, "`"); idx >= 0 {
			close := strings.Index(text[idx+1:], "`")
			if close >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<code>")
				out.WriteString(htmlEscape(text[idx+1 : idx+1+close]))
				out.WriteString("</code>")
				text = text[idx+2+close:]
				continue
			}
		}

		// Link: [text](url)
		if idx := strings.Index(text, "["); idx >= 0 {
			if end, href, linkText, ok := parseLink(text[idx:]); ok {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString(`<a href="`)
				out.WriteString(htmlEscape(href))
				out.WriteString(`">`)
				scanInline(linkText, out)
				out.WriteString("</a>")
				text = text[idx+end:]
				continue
			}
		}

		// Bold: **…** (check before single *)
		if idx := strings.Index(text, "**"); idx >= 0 {
			after := text[idx+2:]
			if end := strings.Index(after, "**"); end >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<b>")
				scanInline(after[:end], out)
				out.WriteString("</b>")
				text = after[end+2:]
				continue
			}
		}

		// Bold: __…__
		if idx := strings.Index(text, "__"); idx >= 0 {
			after := text[idx+2:]
			if end := strings.Index(after, "__"); end >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<b>")
				scanInline(after[:end], out)
				out.WriteString("</b>")
				text = after[end+2:]
				continue
			}
		}

		// Strikethrough: ~~…~~
		if idx := strings.Index(text, "~~"); idx >= 0 {
			after := text[idx+2:]
			if end := strings.Index(after, "~~"); end >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<s>")
				scanInline(after[:end], out)
				out.WriteString("</s>")
				text = after[end+2:]
				continue
			}
		}

		// Italic: *…* (single asterisk, not **)
		if idx := indexSingleStar(text); idx >= 0 {
			after := text[idx+1:]
			if end := indexSingleStar(after); end >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<i>")
				scanInline(after[:end], out)
				out.WriteString("</i>")
				text = after[end+1:]
				continue
			}
		}

		// Italic: _…_ (single underscore)
		if idx := indexSingleUnderscore(text); idx >= 0 {
			after := text[idx+1:]
			if end := indexSingleUnderscore(after); end >= 0 {
				out.WriteString(htmlEscape(text[:idx]))
				out.WriteString("<i>")
				scanInline(after[:end], out)
				out.WriteString("</i>")
				text = after[end+1:]
				continue
			}
		}

		// No more patterns — escape the rest
		out.WriteString(htmlEscape(text))
		break
	}
}

// stripInline removes inline markdown markers from text.
func stripInline(text string) string {
	// Remove inline code backticks
	text = removeWrapped(text, "`", "`")
	// Bold
	text = removeWrapped(text, "**", "**")
	text = removeWrapped(text, "__", "__")
	// Strikethrough
	text = removeWrapped(text, "~~", "~~")
	// Italic (single)
	text = removeSingleWrapped(text, '*')
	text = removeSingleWrapped(text, '_')
	// Links [text](url) → text
	text = stripLinks(text)
	return text
}

// stripHeadingPrefix removes the leading `#…# ` from a heading line and
// returns the trimmed title. Returns the original line unchanged if it is
// not a heading so callers can use identity comparison.
func stripHeadingPrefix(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 {
		return line
	}
	rest := line[i:]
	if len(rest) == 0 || rest[0] != ' ' {
		return line
	}
	return strings.TrimSpace(rest[1:])
}

// unorderedListItem returns the item text if the line is an unordered list
// item (- / * / +), trimming the marker and leading space.
func unorderedListItem(line string) (string, bool) {
	if len(line) < 2 {
		return "", false
	}
	if (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return strings.TrimSpace(line[2:]), true
	}
	return "", false
}

// orderedListItem returns the item text and number if the line is an ordered
// list item (N. text), or ("", 0, false) otherwise.
func orderedListItem(line string) (string, int, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != '.' {
		return "", 0, false
	}
	if i+1 >= len(line) || line[i+1] != ' ' {
		return "", 0, false
	}
	n := 0
	for _, c := range line[:i] {
		n = n*10 + int(c-'0')
	}
	return strings.TrimSpace(line[i+2:]), n, true
}

// isTableRow returns true when the line looks like a GFM table row (| … |).
func isTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") && len(trimmed) > 2
}

// isTableSeparator returns true for table header-separator rows like |---|:---:|.
func isTableSeparator(line string) bool {
	if !isTableRow(line) {
		return false
	}
	inner := strings.TrimSpace(line)
	inner = strings.Trim(inner, "|")
	for _, cell := range strings.Split(inner, "|") {
		cell = strings.TrimSpace(cell)
		if len(cell) == 0 {
			continue
		}
		cell = strings.Trim(cell, ":")
		if strings.TrimLeft(cell, "-") != "" {
			return false
		}
	}
	return true
}

// tableRowText extracts cell text from a GFM table row and joins with spaces.
func tableRowText(line string) string {
	inner := strings.TrimSpace(line)
	inner = strings.Trim(inner, "|")
	cells := strings.Split(inner, "|")
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c != "" {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "  ")
}

// htmlEscape escapes &, <, > in literal text so it is safe inside
// Telegram HTML parse_mode content.
func htmlEscape(s string) string {
	if !strings.ContainsAny(s, "&<>") {
		return s
	}
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// appendNewline writes a newline unless this is the last line.
func appendNewline(out *strings.Builder, i, total int) {
	if i < total-1 {
		out.WriteByte('\n')
	}
}

// parseLink attempts to parse [text](url) starting at s[0]=='['.
// Returns (consumed bytes, href, linkText, true) on success.
func parseLink(s string) (int, string, string, bool) {
	if len(s) < 4 || s[0] != '[' {
		return 0, "", "", false
	}
	closeB := strings.Index(s[1:], "]")
	if closeB < 0 {
		return 0, "", "", false
	}
	closeB++ // relative to s
	if closeB+1 >= len(s) || s[closeB+1] != '(' {
		return 0, "", "", false
	}
	openP := closeB + 1
	closeP := strings.Index(s[openP+1:], ")")
	if closeP < 0 {
		return 0, "", "", false
	}
	closeP += openP + 1
	linkText := s[1:closeB]
	href := s[openP+1 : closeP]
	return closeP + 1, href, linkText, true
}

// indexSingleStar returns the first index of a lone `*` that is not part of
// `**`. Returns -1 if not found.
func indexSingleStar(s string) int {
	for i := 0; i < len(s); {
		if s[i] == '*' {
			if i+1 < len(s) && s[i+1] == '*' {
				i += 2
				continue
			}
			return i
		}
		i++
	}
	return -1
}

// indexSingleUnderscore returns the first index of a lone `_` that is not
// part of `__` and is not adjacent to a word character on both sides
// (avoids breaking snake_case). Returns -1 if not found.
func indexSingleUnderscore(s string) int {
	for i := 0; i < len(s); {
		if s[i] == '_' {
			if i+1 < len(s) && s[i+1] == '_' {
				i += 2
				continue
			}
			// Avoid matching inside snake_case: skip if both neighbours are word chars.
			prevWord := i > 0 && isWordChar(rune(s[i-1]))
			nextWord := i+1 < len(s) && isWordChar(rune(s[i+1]))
			if prevWord && nextWord {
				i++
				continue
			}
			return i
		}
		i++
	}
	return -1
}

func isWordChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// removeWrapped strips all occurrences of open…close pairs.
func removeWrapped(s, open, close string) string {
	for {
		start := strings.Index(s, open)
		if start < 0 {
			break
		}
		end := strings.Index(s[start+len(open):], close)
		if end < 0 {
			break
		}
		end += start + len(open)
		s = s[:start] + s[start+len(open):end] + s[end+len(close):]
	}
	return s
}

// removeSingleWrapped strips lone marker characters (e.g. *text*) while
// skipping doubled markers (**).
func removeSingleWrapped(s string, marker byte) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] == marker {
			if i+1 < len(s) && s[i+1] == marker {
				out.WriteByte(s[i])
				out.WriteByte(s[i+1])
				i += 2
				continue
			}
			i++ // skip the single marker
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// stripLinks converts [text](url) to just text.
func stripLinks(s string) string {
	for {
		start := strings.Index(s, "[")
		if start < 0 {
			break
		}
		consumed, _, linkText, ok := parseLink(s[start:])
		if !ok {
			break
		}
		s = s[:start] + linkText + s[start+consumed:]
	}
	return s
}

// itoa is a minimal int-to-string helper to avoid importing strconv here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
