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
