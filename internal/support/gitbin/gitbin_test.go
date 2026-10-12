package gitbin

import "testing"

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][2]int{
		"git version 2.50.1 (Apple Git-155)\n": {2, 50},
		"git version 2.31.0":                   {2, 31},
		"git version 2.43.0.windows.1":         {2, 43},
	} {
		major, minor, err := ParseVersion(in)
		if err != nil || major != want[0] || minor != want[1] {
			t.Errorf("ParseVersion(%q) = %d.%d, %v", in, major, minor, err)
		}
	}
	if _, _, err := ParseVersion("not git"); err == nil {
		t.Error("garbage was read as a version")
	}
}
