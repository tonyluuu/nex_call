package playout

import "testing"

const fs = 4

type fakeDecoder struct{ plc, fec int }

func (f *fakeDecoder) Decode(p []byte) ([]int16, error) {
	out := make([]int16, fs)
	for i := range out {
		out[i] = int16(p[0])
	}
	return out, nil
}

func (f *fakeDecoder) DecodeFEC(next []byte) ([]int16, error) {
	f.fec++
	out := make([]int16, fs)
	for i := range out {
		out[i] = -int16(next[0])
	}
	return out, nil
}

func (f *fakeDecoder) DecodePLC() ([]int16, error) {
	f.plc++
	return []int16{7, 7, 7, 7}, nil
}

func TestPullAcrossFrameBoundaries(t *testing.T) {
	p := New(&fakeDecoder{}, fs, 2, 10)
	p.Push(1, []byte{1})
	p.Push(2, []byte{2})

	out := make([]int16, 6)
	p.Pull(out)
	want := []int16{1, 1, 1, 1, 2, 2}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("out = %v want %v", out, want)
		}
	}
	p.Pull(out)
	want = []int16{2, 2, 0, 0, 0, 0}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("second pull = %v want %v", out, want)
		}
	}
}

func TestLossUsesFEC(t *testing.T) {
	dec := &fakeDecoder{}
	p := New(dec, fs, 2, 10)
	p.Push(1, []byte{1})
	p.Push(3, []byte{3})

	out := make([]int16, fs*3)
	p.Pull(out)
	if dec.fec != 1 || out[fs] != -3 || out[2*fs] != 3 {
		t.Fatalf("fec=%d out=%v", dec.fec, out)
	}
	if s := p.Stats(); s.FEC != 1 || s.Lost != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestSilenceBeforePrefill(t *testing.T) {
	p := New(&fakeDecoder{}, fs, 3, 10)
	p.Push(1, []byte{9})
	out := make([]int16, fs)
	p.Pull(out)
	for _, s := range out {
		if s != 0 {
			t.Fatalf("expected silence, got %v", out)
		}
	}
}
