// Package jitter 实现按 RTP 序号重排的自适应音频抖动缓冲。
// 由播放端时钟驱动：播放设备每需要一帧就调用一次 Pop。
//
// 自适应策略：出现迟到包说明缓冲太浅，目标深度加一帧并插入一帧补偿音把缓冲撑大；
// 连续一段时间没有迟到包再逐步回落；实际积压明显超过目标时丢弃最旧的帧以控制延迟。
package jitter

import "sync"

type Kind int

const (
	// Empty 缓冲尚未攒够或已经欠载，应输出静音。
	Empty Kind = iota
	// Frame 正常取到一帧。
	Frame
	// Lost 期望的包丢失；FEC 非空时可用下一包的带内 FEC 恢复，否则做 PLC。
	Lost
	// Conceal 为加深缓冲而插入的一帧，应输出 PLC，序号不前进。
	Conceal
)

type Result struct {
	Kind    Kind
	Payload []byte
	FEC     []byte
}

type Stats struct {
	Pushed    uint64
	Played    uint64
	Lost      uint64
	Late      uint64
	Dropped   uint64
	Underruns uint64
	Stretched uint64
	Depth     int
	Target    int
}

const (
	// 积压超过目标 2 帧并持续约 1 秒（50 帧）则丢一帧
	shrinkAfterPops = 50
	// 约 10 秒没有迟到包则目标深度减一帧
	calmAfterPops = 500
)

type Buffer struct {
	mu        sync.Mutex
	minTarget int
	target    int
	maxTarget int
	max       int
	pkts      map[uint16][]byte
	next      uint16
	started   bool
	playing   bool
	stretch   int
	overRun   int
	calm      int
	stats     Stats
}

// New 创建缓冲。target 为初始（也是最小）目标帧数，max 为最大缓存帧数；
// 目标深度最多自适应增长到 max 的一半。
func New(target, maxFrames int) *Buffer {
	if target < 1 {
		target = 1
	}
	if maxFrames <= target {
		maxFrames = target * 4
	}
	return &Buffer{
		minTarget: target,
		target:    target,
		maxTarget: max(target, maxFrames/2),
		max:       maxFrames,
		pkts:      make(map[uint16][]byte, maxFrames),
	}
}

// seqDiff 返回 a-b，正确处理 16 位序号回绕。
func seqDiff(a, b uint16) int {
	return int(int16(a - b))
}

func (b *Buffer) Push(seq uint16, payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.stats.Pushed++
	if !b.started {
		b.reset(seq)
	}
	d := seqDiff(seq, b.next)
	switch {
	case d >= 1000 || d <= -1000:
		// 序号跳变（对端重启或换轨），重新开始
		b.reset(seq)
	case d < 0 && b.playing:
		b.stats.Late++
		b.calm = 0
		if b.target < b.maxTarget {
			b.target++
			b.stretch++
		}
		return
	}
	if _, dup := b.pkts[seq]; dup {
		return
	}
	b.pkts[seq] = payload

	if len(b.pkts) > b.max && !b.playing {
		b.skipToEarliest()
	}
	for len(b.pkts) > b.max {
		b.dropNext()
	}
}

func (b *Buffer) dropNext() {
	if _, ok := b.pkts[b.next]; ok {
		delete(b.pkts, b.next)
		b.stats.Dropped++
	}
	b.next++
}

func (b *Buffer) reset(seq uint16) {
	clear(b.pkts)
	b.next = seq
	b.started = true
	b.playing = false
	b.stretch = 0
	b.overRun = 0
}

func (b *Buffer) Pop() Result {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.playing {
		if len(b.pkts) < b.target {
			return Result{Kind: Empty}
		}
		b.playing = true
		b.skipToEarliest()
	}

	if b.stretch > 0 {
		b.stretch--
		b.stats.Stretched++
		return Result{Kind: Conceal}
	}
	b.adapt()

	if p, ok := b.pkts[b.next]; ok {
		delete(b.pkts, b.next)
		b.next++
		b.stats.Played++
		return Result{Kind: Frame, Payload: p}
	}

	if len(b.pkts) == 0 {
		b.playing = false
		b.stats.Underruns++
		return Result{Kind: Empty}
	}

	b.stats.Lost++
	fec := b.pkts[b.next+1]
	b.next++
	return Result{Kind: Lost, FEC: fec}
}

func (b *Buffer) adapt() {
	if len(b.pkts) > b.target+2 {
		b.overRun++
		if b.overRun >= shrinkAfterPops {
			b.dropNext()
			b.overRun = 0
		}
	} else {
		b.overRun = 0
	}
	b.calm++
	if b.calm >= calmAfterPops {
		b.calm = 0
		if b.target > b.minTarget {
			b.target--
		}
	}
}

// skipToEarliest 起播时从缓冲中最早的包开始，避免把欠载期间的空洞当作丢包。
func (b *Buffer) skipToEarliest() {
	first := true
	var earliest uint16
	for s := range b.pkts {
		if first || seqDiff(s, earliest) < 0 {
			earliest = s
			first = false
		}
	}
	if !first {
		b.next = earliest
	}
}

func (b *Buffer) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats
	s.Depth = len(b.pkts)
	s.Target = b.target
	return s
}
