package agent

import "testing"

const def = "http://default.invalid/golden.img.zst"

func TestSelectMode(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		def      string
		wantMode Mode
		wantURL  string
	}{
		{"no flags", nil, def, ModeNormal, ""},
		{"no flags, no default", map[string]string{}, "", ModeNormal, ""},
		{"reflash empty uses default", map[string]string{FlagReflash: ""}, def, ModeReflash, def},
		{"reflash whitespace uses default", map[string]string{FlagReflash: "\n\n"}, def, ModeReflash, def},
		{"reflash url overrides", map[string]string{FlagReflash: "http://a/b.zst\n"}, def, ModeReflash, "http://a/b.zst"},
		{"crlf trimmed", map[string]string{FlagReflash: "http://a/b.zst\r\nextra\n"}, def, ModeReflash, "http://a/b.zst"},
		{"spaces trimmed", map[string]string{FlagReflash: "  http://a/b.zst  "}, def, ModeReflash, "http://a/b.zst"},
		{"reflash without any url is an error", map[string]string{FlagReflash: ""}, "", ModeError, ""},
		{"capture", map[string]string{FlagCapture: "http://mac:8080/capture?mac=x"}, def, ModeCapture, "http://mac:8080/capture?mac=x"},
		{"dryrun", map[string]string{FlagDryrun: "http://a/b.zst"}, def, ModeDryrun, "http://a/b.zst"},
		{"dryrun beats capture and reflash", map[string]string{
			FlagDryrun: "http://d/", FlagCapture: "http://c/", FlagReflash: "http://r/",
		}, def, ModeDryrun, "http://d/"},
		{"capture beats reflash", map[string]string{
			FlagCapture: "http://c/", FlagReflash: "http://r/",
		}, def, ModeCapture, "http://c/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, url := SelectMode(tc.files, tc.def)
			if mode != tc.wantMode || url != tc.wantURL {
				t.Fatalf("SelectMode = (%s, %q), want (%s, %q)", mode, url, tc.wantMode, tc.wantURL)
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"\n":      "",
		"a":       "a",
		"a\nb":    "a",
		"a\r\nb":  "a",
		"  a  \n": "a",
		"\r\n":    "",
	}
	for in, want := range cases {
		if got := FirstLine(in); got != want {
			t.Errorf("FirstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNeedsNetwork(t *testing.T) {
	for _, m := range []Mode{ModeReflash, ModeDryrun, ModeCapture} {
		if !m.NeedsNetwork() {
			t.Errorf("%s should need network", m)
		}
	}
	for _, m := range []Mode{ModeNormal, ModeError} {
		if m.NeedsNetwork() {
			t.Errorf("%s should not need network", m)
		}
	}
}
