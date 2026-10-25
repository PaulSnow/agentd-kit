package main

import (
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// Markdown for the UI (#41): a small renderer over escaped text, enough
// for STATUS.md and the questions sessions ask. Paragraphs, ATX headings,
// lists, fenced code, code spans, bold, and links whose target is http or
// https. Every byte of the input is escaped before any tag is added, so
// no raw HTML passes.

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	mdItem    = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*)$`)
	mdCode    = regexp.MustCompile("`([^`]+)`")
	mdBold    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	// A link's text and target, both already escaped; the target must be
	// http or https and hold no quote or space.
	mdLink = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s()"&\x00]+(?:&amp;[^\s()"&\x00]+)*)\)`)
)

// markdown renders text as HTML.
func markdown(text string) template.HTML {
	var b strings.Builder
	var para, items []string
	fence := false
	var code []string
	flushPara := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + mdInline(strings.Join(para, " ")) + "</p>\n")
			para = nil
		}
	}
	flushList := func() {
		if len(items) > 0 {
			b.WriteString("<ul>")
			for _, it := range items {
				b.WriteString("<li>" + mdInline(it) + "</li>")
			}
			b.WriteString("</ul>\n")
			items = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if fence {
				b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
				code, fence = nil, false
			} else {
				flushPara()
				flushList()
				fence = true
			}
			continue
		}
		if fence {
			code = append(code, line)
			continue
		}
		switch m := mdHeading.FindStringSubmatch(trimmed); {
		case trimmed == "":
			flushPara()
			flushList()
		case m != nil:
			flushPara()
			flushList()
			// Headings inside a card start at h3.
			level := len(m[1]) + 2
			if level > 6 {
				level = 6
			}
			tag := "h" + strconv.Itoa(level)
			b.WriteString("<" + tag + ">" + mdInline(m[2]) + "</" + tag + ">\n")
		case mdItem.MatchString(line):
			flushPara()
			items = append(items, mdItem.FindStringSubmatch(line)[1])
		case len(items) > 0 && strings.HasPrefix(line, " "):
			items[len(items)-1] += " " + trimmed // a list item's continuation
		default:
			flushList()
			para = append(para, trimmed)
		}
	}
	if fence {
		b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
	}
	flushPara()
	flushList()
	return template.HTML(b.String())
}

// mdInline renders one line's code spans, bold and links, after escaping
// it. Code spans are set aside first, so nothing inside one is read as
// bold or a link, then links.
func mdInline(s string) string {
	s = html.EscapeString(strings.ReplaceAll(s, "\x00", ""))
	var spans []string
	s = mdCode.ReplaceAllStringFunc(s, func(m string) string {
		spans = append(spans, "<code>"+mdCode.FindStringSubmatch(m)[1]+"</code>")
		return "\x00" + strconv.Itoa(len(spans)-1) + "\x00"
	})
	// Links are set aside before bold, so no tag lands in a target; a
	// target holding a code span is no link.
	s = mdLink.ReplaceAllStringFunc(s, func(m string) string {
		l := mdLink.FindStringSubmatch(m)
		spans = append(spans, `<a href="`+l[2]+`" rel="noreferrer">`+mdBold.ReplaceAllString(l[1], "<strong>$1</strong>")+`</a>`)
		return "\x00" + strconv.Itoa(len(spans)-1) + "\x00"
	})
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	// Latest first: a link's text may hold an earlier code span.
	for i := len(spans) - 1; i >= 0; i-- {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", spans[i], 1)
	}
	return s
}
