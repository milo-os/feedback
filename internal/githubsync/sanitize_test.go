package githubsync

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeHTML(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		// want is the exact output, unless partial is set.
		want    string
		partial bool
		// has and hasNot are substrings that must or must not appear.
		has, hasNot []string
	}{
		{
			name:   "script dropped with content",
			in:     `<p>hi</p><script>alert(1)</script><style>p{color:red}</style>`,
			want:   `<p>hi</p>`,
			hasNot: []string{"alert", "color"},
		},
		{
			name:   "event handlers removed",
			in:     `<p onclick="alert(1)">x</p><img src="https://example.com/a.png" onerror="alert(1)">`,
			want:   `<p>x</p><img src="https://example.com/a.png">`,
			hasNot: []string{"onclick", "onerror"},
		},
		{
			name:   "javascript href removed, text kept",
			in:     `<a href="javascript:alert(1)">click</a>`,
			want:   `click`,
			hasNot: []string{"href", "javascript"},
		},
		{
			name:   "data href removed, text kept",
			in:     `<a href="data:text/html;base64,PHNjcmlwdD4=">click</a>`,
			want:   `click`,
			hasNot: []string{"data:"},
		},
		{
			name: "relative and fragment hrefs removed, text kept",
			in:   `<a href="/foo">one</a> <a href="#bar">two</a> <a href="//evil.example/x">three</a>`,
			want: `one two three`,
		},
		{
			name:    "external link gets rel and target",
			partial: true,
			in:      `<a href="https://example.com/x" rel="opener" target="_self" class="c">x</a>`,
			has: []string{
				`href="https://example.com/x"`, `target="_blank"`,
				"nofollow", "noopener", "noreferrer",
			},
			hasNot: []string{"opener ", "_self", "class"},
		},
		{
			name:    "mailto link kept",
			partial: true,
			in:      `<a href="mailto:a@example.com">mail</a>`,
			has:     []string{`href="mailto:a@example.com"`, "nofollow", "noreferrer"},
		},
		{
			name:   "http img dropped",
			in:     `<p>a<img src="http://example.com/a.png" alt="x">b</p>`,
			want:   `<p>ab</p>`,
			hasNot: []string{"img"},
		},
		{
			name:   "data img dropped",
			in:     `<img src="data:image/png;base64,AAAA">`,
			want:   ``,
			hasNot: []string{"data:"},
		},
		{
			name: "https img kept with allowed attributes",
			in:   `<img src="https://camo.githubusercontent.com/abc" alt="shot" title="t" width="100" height="50px" style="x" data-canonical-src="http://x">`,
			want: `<img src="https://camo.githubusercontent.com/abc" alt="shot" title="t" width="100">`,
		},
		{
			name: "checkbox kept and disabled",
			in:   `<ul class="contains-task-list"><li class="task-list-item"><input type="checkbox" id="x" class="task-list-item-checkbox" checked> done</li></ul>`,
			want: `<ul><li><input type="checkbox" checked="" disabled=""/> done</li></ul>`,
		},
		{
			name: "unchecked checkbox disabled",
			in:   `<input type="checkbox">`,
			want: `<input type="checkbox" disabled=""/>`,
		},
		{
			name: "other inputs dropped",
			in:   `<input type="text" value="x"><input type="password"><input checked><input disabled><input type="CHECKBOX">ok`,
			want: `ok`,
		},
		{
			name:   "form elements dropped",
			in:     `<form action="https://example.com"><button>go</button></form><iframe src="https://example.com"></iframe><object data="x"></object><embed src="x">`,
			want:   `go`,
			hasNot: []string{"form", "iframe", "object", "embed", "button"},
		},
		{
			name: "class style and id stripped",
			in:   `<div class="highlight highlight-source-go" id="x" style="color:red"><pre><code class="language-go">x := 1</code></pre></div>`,
			want: `<div><pre><code>x := 1</code></pre></div>`,
		},
		{
			name: "details summary and table kept",
			in:   `<details open><summary>More</summary><table><thead><tr><th align="left">a</th><th align="javascript">b</th></tr></thead><tbody><tr><td align="right">1</td><td>2</td></tr></tbody></table></details>`,
			want: `<details open=""><summary>More</summary><table><thead><tr><th align="left">a</th><th>b</th></tr></thead><tbody><tr><td align="right">1</td><td>2</td></tr></tbody></table></details>`,
		},
		{
			name: "ordered list start",
			in:   `<ol start="3"><li>x</li></ol><ol start="x"><li>y</li></ol>`,
			want: `<ol start="3"><li>x</li></ol><ol><li>y</li></ol>`,
		},
		{
			name: "g-emoji text kept",
			in:   `<p>ship it <g-emoji class="g-emoji" alias="rocket" fallback-src="https://x">🚀</g-emoji></p>`,
			want: `<p>ship it 🚀</p>`,
		},
		{
			name: "svg dropped with content",
			in:   `<p><svg viewBox="0 0 16 16"><path d="M0 0"></path><text>junk</text></svg>Note</p>`,
			want: `<p>Note</p>`,
		},
		{
			name: "inline formatting kept",
			in:   `<p><strong>a</strong><em>b</em><del>c</del><kbd>d</kbd><sup>e</sup><mark>f</mark></p><hr><blockquote>q</blockquote>`,
			want: `<p><strong>a</strong><em>b</em><del>c</del><kbd>d</kbd><sup>e</sup><mark>f</mark></p><hr><blockquote>q</blockquote>`,
		},
		{
			name: "empty",
			in:   ``,
			want: ``,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeHTML(tc.in)
			if !tc.partial && got != tc.want {
				t.Errorf("SanitizeHTML() = %q, want %q", got, tc.want)
			}
			for _, s := range tc.has {
				if !strings.Contains(got, s) {
					t.Errorf("SanitizeHTML() = %q, want it to contain %q", got, s)
				}
			}
			for _, s := range tc.hasNot {
				if strings.Contains(got, s) {
					t.Errorf("SanitizeHTML() = %q, want it not to contain %q", got, s)
				}
			}
		})
	}
}

func TestSanitizeHTMLOversized(t *testing.T) {
	para := "<p>" + strings.Repeat("a", 1000) + "</p>"
	under := strings.Repeat(para, (MaxBodyHTML-1)/len(para))
	if got := SanitizeHTML(under); got != under {
		t.Errorf("SanitizeHTML() of %d bytes changed or dropped the body", len(under))
	}
	over := strings.Repeat(para, MaxBodyHTML/len(para)+1)
	if got := SanitizeHTML(over); got != "" {
		t.Errorf("SanitizeHTML() of %d bytes = %d bytes, want \"\"", len(over), len(got))
	}
}

func TestSummary(t *testing.T) {
	word := "abcdefghi " // 10 runes
	long := strings.Repeat(word, 40)

	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "only whitespace", in: " \n\t\r ", want: ""},
		{name: "short unchanged", in: "Add IPv6 support.", want: "Add IPv6 support."},
		{
			name: "whitespace collapsed",
			in:   "  First line\n\nsecond\tline   end  \n",
			want: "First line second line end",
		},
		{
			name: "control characters stripped",
			in:   "a\x00b\x1bc\x7fd",
			want: "abcd",
		},
		{
			name: "invalid utf-8 dropped",
			in:   "a\xffb",
			want: "ab",
		},
		{
			name: "exactly SummaryLength unchanged",
			in:   strings.Repeat("x", SummaryLength),
			want: strings.Repeat("x", SummaryLength),
		},
		{
			name: "cut at word boundary",
			in:   long,
			// 28 words fill 280 runes; the space at rune 279 ends the 28th.
			want: strings.TrimSpace(strings.Repeat(word, 28)) + "…",
		},
		{
			name: "cut before a word that crosses the limit",
			in:   strings.Repeat("x", 270) + " " + strings.Repeat("y", 30),
			want: strings.Repeat("x", 270) + "…",
		},
		{
			name: "hard cut with no space near the end",
			in:   strings.Repeat("x", 200) + " " + strings.Repeat("y", 200),
			want: strings.Repeat("x", 200) + " " + strings.Repeat("y", SummaryLength-201) + "…",
		},
		{
			name: "multibyte runes not split",
			in:   strings.Repeat("é", 300),
			want: strings.Repeat("é", SummaryLength) + "…",
		},
		{
			name: "emoji not split",
			in:   strings.Repeat("🚀", 281),
			want: strings.Repeat("🚀", SummaryLength) + "…",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Summary(tc.in)
			if got != tc.want {
				t.Errorf("Summary() = %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("Summary() = %q is not valid UTF-8", got)
			}
			if n := utf8.RuneCountInString(got); n > SummaryLength+1 {
				t.Errorf("Summary() is %d runes, want at most %d", n, SummaryLength+1)
			}
		})
	}
}
