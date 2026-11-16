package screen

import "testing"

func TestSanitize(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain text", "hello\nworld", "hello\nworld"},
		{"sgr kept", "\x1b[1;31mred\x1b[0m ok \x1b[38;2;1;2;3mx\x1b[m", "\x1b[1;31mred\x1b[0m ok \x1b[38;2;1;2;3mx\x1b[m"},
		{"sgr with colons", "\x1b[38:5:9mx", "\x1b[38:5:9mx"},
		{"cursor movement", "a\x1b[2Jb\x1b[10;5Hc\x1b[Ad\x1b[1;1fe", "abcde"},
		{"erase and scroll", "a\x1b[Kb\x1b[2Sc\x1b[3Td", "abcd"},
		{"private modes", "a\x1b[?1049hb\x1b[?25lc\x1b[?2004h", "abc"},
		{"sgr lookalike with private marker", "a\x1b[?1mb\x1b[>4;1mc", "abc"},
		{"sgr with intermediate", "a\x1b[1 mb", "ab"},
		{"device queries", "a\x1b[6nb\x1b[cc\x1b[>cd", "abcd"},
		{"osc title bel", "a\x1b]0;evil title\x07b", "ab"},
		{"osc title st", "a\x1b]2;evil\x1b\\b", "ab"},
		{"osc 52 clipboard bel", "a\x1b]52;c;aGVsbG8=\x07b", "ab"},
		{"osc 52 clipboard st", "a\x1b]52;c;aGVsbG8=\x1b\\b", "ab"},
		{"osc 8 hyperlink", "\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\", "link"},
		{"dcs", "a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},
		{"dcs with bel inside is not ended by it", "a\x1bPdata\x07more\x1b\\b", "ab"},
		{"apc pm sos", "a\x1b_app\x1b\\b\x1b^pm\x1b\\c\x1bXsos\x1b\\d", "abcd"},
		{"unterminated osc eats the rest", "a\x1b]52;c;AAAA\nsecret", "a"},
		{"unterminated dcs eats the rest", "a\x1bPxxx\nsecret", "a"},
		{"esc in a string restarts", "a\x1b]0;t\x1b[31mred", "a\x1b[31mred"},
		{"charset and keypad", "a\x1b(0b\x1b=c\x1b>d\x1b7e\x1b8f\x1bcg", "abcdefg"},
		{"trailing lone esc", "ab\x1b", "ab"},
		{"truncated csi", "ab\x1b[31", "ab"},
		{"csi aborted by a control", "a\x1b[3\x01mb", "amb"},
		{"c1 csi 8-bit", "a\u009b2Jb", "ab"},
		{"c1 osc 8-bit", "a\u009d52;c;AAAA\u009cb", "ab"},
		{"c1 dcs 8-bit", "a\u0090q\u009cb", "ab"},
		{"c1 alone", "a\u0085b", "ab"},
		{"invalid utf8 becomes replacement", "a\x9bb\xffc", "a�b�c"},
		{"utf8 kept", "ñ 日本 😀", "ñ 日本 😀"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"lone cr", "a\rb", "a\nb"},
		{"other controls dropped", "a\x00b\x07c\x08d\x0ce\x7ff\tg", "abcdef\tg"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(Sanitize([]byte(c.in))); got != c.want {
				t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeLeavesNoEscapeButSGR(t *testing.T) {
	in := []byte("x\x1b]52;c;QQ==\x07\x1b[31my\x1b[2J\x1b]0;t\x1b\\\x1bPz\x1b\\\x1b[?1049h\x1b[0m")
	out := Sanitize(in)
	for i := 0; i < len(out); i++ {
		if out[i] == 0x1b && (i+1 >= len(out) || out[i+1] != '[') {
			t.Fatalf("an escape that is not CSI survived: %q", out)
		}
	}
	if string(out) != "x\x1b[31my\x1b[0m" {
		t.Errorf("got %q", out)
	}
}
