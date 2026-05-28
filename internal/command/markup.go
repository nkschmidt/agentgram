package command

import (
	"fmt"
	"regexp"
	"strings"
)

// MarkdownToHTML converts a subset of Markdown to Telegram HTML.
// Telegram supports a narrow set of tags: <b>, <i>, <u>, <s>, <code>,
// <pre>, <a>, <blockquote>. Everything else we either simplify to these,
// or leave as plain text.
//
// Conversion is done in order:
//  1. Extract code-blocks (triple backtick) and inline code (single) —
//     hide them behind placeholders so nothing inside gets interpreted.
//  2. Escape `&<>` in the remaining text.
//  3. Apply markdown replacements (bold/italic/strike/headers/links/bullets).
//  4. Restore code-blocks into the text, with escaping inside them.
//
// No library: simple regex is enough for the most frequent constructs
// in claude/opencode output. Edge cases (escaped \*, nested formatting)
// are skipped — Telegram will reject broken HTML, and the caller will
// fall back to plain.
func MarkdownToHTML(s string) string {
	type codeBlock struct {
		lang    string
		content string
	}
	var blocks []codeBlock
	var inlines []string

	// 1a. Triple backtick. (?s) — dot matches \n.
	s = reCodeBlock.ReplaceAllStringFunc(s, func(m string) string {
		sub := reCodeBlock.FindStringSubmatch(m)
		blocks = append(blocks, codeBlock{lang: sub[1], content: sub[2]})
		return fmt.Sprintf("\x00B%d\x00", len(blocks)-1)
	})

	// 1b. Single backtick (no newlines inside — otherwise we confuse with code-blocks).
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		sub := reInlineCode.FindStringSubmatch(m)
		inlines = append(inlines, sub[1])
		return fmt.Sprintf("\x00I%d\x00", len(inlines)-1)
	})

	// 2. HTML escape.
	s = htmlEscape(s)

	// 3. Markdown → HTML.
	// **bold** / __bold__
	s = reBoldStar.ReplaceAllString(s, "<b>$1</b>")
	s = reBoldUnder.ReplaceAllString(s, "<b>$1</b>")
	// ~~strike~~
	s = reStrike.ReplaceAllString(s, "<s>$1</s>")
	// *italic*  — after **bold** (otherwise **a** conflicts)
	s = reItalicStar.ReplaceAllString(s, "<i>$1</i>")
	// _italic_ intentionally NOT touched — `_` is often part of identifiers
	// (snake_case, /new_session etc).
	// # Header (any level) → <b>
	s = reHeader.ReplaceAllString(s, "<b>$1</b>")
	// Lists `- item` / `* item` → "• item".
	s = reBullet.ReplaceAllString(s, "• $1")
	// [text](url) → <a>.
	s = reLink.ReplaceAllString(s, `<a href="$2">$1</a>`)

	// 4a. Restore inline code (with escape inside).
	for i, c := range inlines {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00I%d\x00", i),
			"<code>"+htmlEscape(c)+"</code>")
	}
	// 4b. Restore block code.
	for i, b := range blocks {
		inner := "<pre><code"
		if b.lang != "" {
			inner += ` class="language-` + htmlEscape(b.lang) + `"`
		}
		inner += ">" + htmlEscape(b.content) + "</code></pre>"
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00B%d\x00", i), inner)
	}

	return s
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

var (
	reCodeBlock  = regexp.MustCompile("(?s)```(\\w*)\\n?(.*?)```")
	reInlineCode = regexp.MustCompile("`([^`\\n]+)`")
	reBoldStar   = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reBoldUnder  = regexp.MustCompile(`__([^_\n]+)__`)
	reItalicStar = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrike     = regexp.MustCompile(`~~([^~\n]+)~~`)
	reHeader     = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reBullet     = regexp.MustCompile(`(?m)^[*\-]\s+(.+)$`)
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)
