package tools

import "testing"

func TestNormalizeJSON(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"cjk-string", `"中文"`, `"中文"`},
		{"cjk-object", `{"t":"\u4e2d\u6587"}`, `{"t":"中文"}`},
		{"html-chars-kept-literal", `"<a>&"`, `"<a>&"`},
		{"astral-surrogate", `"\ud83d\ude00"`, `"😀"`},
		{"bigint-preserved", `{"n":12345678901234567890}`, `{"n":12345678901234567890}`},
		{"ascii-esc-unchanged", `"a\nb"`, `"a\nb"`},
		{"invalid-passthrough", `not json`, `not json`},
		{"empty", ``, ``},
	}
	for _, c := range cases {
		got := string(normalizeJSON([]byte(c.in)))
		if got != c.want {
			t.Errorf("%s: normalizeJSON(%s) = %s, want %s", c.name, c.in, got, c.want)
		}
	}
}

func TestJSONUTF8NoHTMLEscape(t *testing.T) {
	b, err := jsonUTF8(map[string]string{"x": "<a>&中"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"x":"<a>&中"}`; got != want {
		t.Errorf("jsonUTF8 = %s, want %s", got, want)
	}
}
