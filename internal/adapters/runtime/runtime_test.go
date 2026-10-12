package runtime

import "testing"

func TestParseName_ReadsOnlyWhatNameForWrites(t *testing.T) {
	run, epoch, ok := ParseName(NameFor("run_01J00000000000000000000000", 7))
	if !ok || run != "run_01J00000000000000000000000" || epoch != 7 {
		t.Fatalf("ParseName = %q %d %v", run, epoch, ok)
	}
	for _, bad := range []string{"cf-run-e0", "cf-run-e07", "cf--e1", "run-e1", "cf-run", "cf-run-ex", "cf-run-e1 "} {
		if _, _, ok := ParseName(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}
