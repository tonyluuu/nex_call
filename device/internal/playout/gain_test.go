package playout

import "testing"

func TestApplyGainLinearBelowKnee(t *testing.T) {
	buf := []int16{1000, -1000, 0}
	ApplyGain(buf, 2)
	if buf[0] != 2000 || buf[1] != -2000 || buf[2] != 0 {
		t.Fatalf("got %v", buf)
	}
}

func TestApplyGainSoftLimits(t *testing.T) {
	buf := []int16{20000, -20000, 32767}
	ApplyGain(buf, 4)
	for i, v := range buf {
		if v > 32767 || v < -32768 {
			t.Fatalf("sample %d out of range: %d", i, v)
		}
	}
	if buf[0] <= 22937 || buf[1] >= -22937 {
		t.Fatalf("expected values above knee, got %v", buf)
	}
	if d := int(buf[0]) + int(buf[1]); d < -1 || d > 1 {
		t.Fatalf("expected symmetric limiting, got %v", buf)
	}
}

func TestApplyGainUnity(t *testing.T) {
	buf := []int16{123, -456}
	ApplyGain(buf, 1)
	if buf[0] != 123 || buf[1] != -456 {
		t.Fatalf("got %v", buf)
	}
}
