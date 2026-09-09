package app

import "testing"

// initGateOpen must open the gate once every object of the startup snapshot
// was counted: an overshoot (an object created after the snapshot also
// counts) must never wedge the startup, and an untercounted object must keep
// the gate waiting.
func TestInitGateOpen(t *testing.T) {
	cases := []struct {
		current int
		target  int
		want    bool
	}{
		{0, 0, true},
		{1, 2, false},
		{2, 2, true},
		{3, 2, true},
	}
	for _, tc := range cases {
		if got := initGateOpen(tc.current, tc.target); got != tc.want {
			t.Errorf("initGateOpen(%d, %d) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}
