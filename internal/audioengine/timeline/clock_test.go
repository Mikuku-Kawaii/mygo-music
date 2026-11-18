package timeline

import "testing"

// Device ticks need a frequency conversion, and old epochs cannot advance a
// new stream. Submitted data caps presentation even if a driver clock runs on.
func TestClockMapping(t *testing.T) {
	c, err := NewClock(7, 100000, 20, 10000000, 48000)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct{ pos, submitted, want uint64 }{{10100000, 100000, 48020}, {10100000, 200, 200}, {100000, 200, 20}} {
		got, err := c.Presented(7, x.pos, x.submitted)
		if err != nil || got != x.want {
			t.Fatal(got, err, x)
		}
	}
	if _, err = c.Presented(6, 100000, 1000); err == nil {
		t.Fatal("accepted old epoch")
	}
	if _, err = c.Presented(7, 99999, 1000); err == nil {
		t.Fatal("accepted clock regression")
	}
}
