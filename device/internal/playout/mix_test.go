package playout

import "testing"

func TestMixAddsAndClips(t *testing.T) {
	dst := make([]int16, 3)
	Mix(dst, [][]int16{
		{100, 30000, -20000},
		{50, 20000, -20000},
	})
	if dst[0] != 150 || dst[1] != 32767 || dst[2] != -32768 {
		t.Fatalf("got %v", dst)
	}
}

func TestMixSingleAndShort(t *testing.T) {
	dst := make([]int16, 2)
	Mix(dst, [][]int16{{7}})
	if dst[0] != 7 || dst[1] != 0 {
		t.Fatalf("got %v", dst)
	}
}
