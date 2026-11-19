package jitter

import "testing"

func payload(seq uint16) []byte { return []byte{byte(seq >> 8), byte(seq)} }

func TestPrefillThenInOrder(t *testing.T) {
	b := New(3, 10)
	b.Push(100, payload(100))
	b.Push(101, payload(101))
	if r := b.Pop(); r.Kind != Empty {
		t.Fatalf("prefill: got %v want Empty", r.Kind)
	}
	b.Push(102, payload(102))
	for i := uint16(100); i <= 102; i++ {
		r := b.Pop()
		if r.Kind != Frame || r.Payload[1] != byte(i) {
			t.Fatalf("seq %d: got %+v", i, r)
		}
	}
	if r := b.Pop(); r.Kind != Empty {
		t.Fatalf("underrun: got %v", r.Kind)
	}
	if b.Stats().Underruns != 1 {
		t.Fatalf("underruns = %d", b.Stats().Underruns)
	}
}

func TestReorder(t *testing.T) {
	b := New(3, 10)
	for _, s := range []uint16{11, 10, 13, 12} {
		b.Push(s, payload(s))
	}
	for i := uint16(10); i <= 13; i++ {
		r := b.Pop()
		if r.Kind != Frame || r.Payload[1] != byte(i) {
			t.Fatalf("seq %d: got %+v", i, r)
		}
	}
}

func TestLossWithFEC(t *testing.T) {
	b := New(2, 10)
	b.Push(1, payload(1))
	b.Push(3, payload(3))
	b.Push(4, payload(4))

	if r := b.Pop(); r.Kind != Frame {
		t.Fatalf("want frame 1, got %+v", r)
	}
	r := b.Pop()
	if r.Kind != Lost || r.FEC == nil || r.FEC[1] != 3 {
		t.Fatalf("want lost with FEC from 3, got %+v", r)
	}
	if r := b.Pop(); r.Kind != Frame || r.Payload[1] != 3 {
		t.Fatalf("want frame 3, got %+v", r)
	}
	if b.Stats().Lost != 1 {
		t.Fatalf("lost = %d", b.Stats().Lost)
	}
}

func TestLateDropped(t *testing.T) {
	b := New(1, 10)
	b.Push(5, payload(5))
	b.Pop()
	b.Push(4, payload(4))
	if b.Stats().Late != 1 {
		t.Fatalf("late = %d", b.Stats().Late)
	}
}

func TestWraparound(t *testing.T) {
	b := New(2, 10)
	for _, s := range []uint16{65534, 65535, 0, 1} {
		b.Push(s, payload(s))
	}
	want := []uint16{65534, 65535, 0, 1}
	for _, w := range want {
		r := b.Pop()
		if r.Kind != Frame || r.Payload[1] != byte(w) {
			t.Fatalf("seq %d: got %+v", w, r)
		}
	}
}

func TestOverflowDropsOldest(t *testing.T) {
	b := New(2, 4)
	for s := uint16(0); s < 8; s++ {
		b.Push(s, payload(s))
	}
	if d := b.Stats().Depth; d != 4 {
		t.Fatalf("depth = %d", d)
	}
	r := b.Pop()
	if r.Kind != Frame || r.Payload[1] != 4 {
		t.Fatalf("want oldest kept = 4, got %+v", r)
	}
}

func TestSequenceJumpResets(t *testing.T) {
	b := New(1, 10)
	b.Push(10, payload(10))
	b.Pop()
	b.Push(30000, payload(30000))
	r := b.Pop()
	if r.Kind != Frame {
		t.Fatalf("want frame after reset, got %+v", r)
	}
}

func TestLateGrowsTargetAndStretches(t *testing.T) {
	b := New(2, 20)
	b.Push(10, payload(10))
	b.Push(11, payload(11))
	b.Pop()
	b.Pop()
	b.Push(12, payload(12))
	b.Push(9, payload(9)) // 迟到
	if s := b.Stats(); s.Late != 1 || s.Target != 3 {
		t.Fatalf("stats %+v", s)
	}
	if r := b.Pop(); r.Kind != Conceal {
		t.Fatalf("want conceal, got %+v", r)
	}
	if r := b.Pop(); r.Kind != Frame || r.Payload[1] != 12 {
		t.Fatalf("want 12 after conceal, got %+v", r)
	}
}

func TestTargetCapped(t *testing.T) {
	b := New(2, 8)
	b.Push(100, payload(100))
	b.Push(101, payload(101))
	b.Pop()
	for i := 0; i < 10; i++ {
		b.Push(50, payload(50))
	}
	if s := b.Stats(); s.Target != 4 {
		t.Fatalf("target = %d, want capped at 4", s.Target)
	}
}

func TestBacklogShrinks(t *testing.T) {
	b := New(2, 100)
	seq := uint16(0)
	for ; seq < 10; seq++ {
		b.Push(seq, payload(seq))
	}
	// 每次 Pop 再补一包，积压保持在目标之上
	for i := 0; i < shrinkAfterPops; i++ {
		b.Pop()
		b.Push(seq, payload(seq))
		seq++
	}
	if s := b.Stats(); s.Dropped != 1 {
		t.Fatalf("dropped = %d, want 1", s.Dropped)
	}
}

func TestTargetDecaysWhenCalm(t *testing.T) {
	b := New(2, 40)
	b.Push(10, payload(10))
	b.Push(11, payload(11))
	b.Pop()
	b.Push(5, payload(5))
	if b.Stats().Target != 3 {
		t.Fatal("target should grow")
	}
	seq := uint16(11)
	for i := 0; i < calmAfterPops+2; i++ {
		b.Push(seq+1, payload(seq+1))
		b.Pop()
		seq++
	}
	if s := b.Stats(); s.Target != 2 {
		t.Fatalf("target = %d, want decayed to 2", s.Target)
	}
}

func TestResumeAfterDTXGap(t *testing.T) {
	b := New(2, 10)
	b.Push(1, payload(1))
	b.Push(2, payload(2))
	b.Pop()
	b.Pop()
	b.Pop() // underrun
	b.Push(3, payload(3))
	b.Push(4, payload(4))
	if r := b.Pop(); r.Kind != Frame || r.Payload[1] != 3 {
		t.Fatalf("want 3 after resume, got %+v", r)
	}
	if b.Stats().Lost != 0 {
		t.Fatalf("lost = %d", b.Stats().Lost)
	}
}
