package main

import (
	"strings"
	"testing"
)

// Markdown renders the common forms and lets no HTML through: tags and
// attributes are escaped, and a link is kept only to http or https.
func TestMarkdown(t *testing.T) {
	got := string(markdown("# Plan\n\nSome **bold** and `<b>code</b>`.\n- one\n- two\n  more\n\n```\n<script>x</script>\n```\n[ok](https://example.com/a?b=1&c=2) [bad](javascript:alert(1)) <img src=x onerror=y>"))
	for _, want := range []string{
		"<h3>Plan</h3>",
		"<p>Some <strong>bold</strong> and <code>&lt;b&gt;code&lt;/b&gt;</code>.</p>",
		"<ul><li>one</li><li>two more</li></ul>",
		"<pre><code>&lt;script&gt;x&lt;/script&gt;</code></pre>",
		`<a href="https://example.com/a?b=1&amp;c=2" rel="noreferrer">ok</a>`,
		"[bad](javascript:alert(1))",
		"&lt;img src=x onerror=y&gt;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "<script") || strings.Contains(got, "<img") || strings.Contains(got, "javascript:alert(1)\"") {
		t.Fatalf("HTML passed:\n%s", got)
	}
}

// Nothing inline lands in a link's target: a target holding a code span
// is no link, bold inside a target stays text, and a link's text may hold
// a code span or bold.
func TestMarkdownLinkTargets(t *testing.T) {
	for in, want := range map[string]string{
		"[`a`](https://x.example/`b`)":              "<p>[<code>a</code>](https://x.example/<code>b</code>)</p>\n",
		"[see](https://x.example/**b**)":            `<p><a href="https://x.example/**b**" rel="noreferrer">see</a></p>` + "\n",
		"[`code` and **bold**](https://x.example/)": `<p><a href="https://x.example/" rel="noreferrer"><code>code</code> and <strong>bold</strong></a></p>` + "\n",
	} {
		if got := string(markdown(in)); got != want {
			t.Errorf("%q:\n%s\nwant\n%s", in, got, want)
		}
	}
}
