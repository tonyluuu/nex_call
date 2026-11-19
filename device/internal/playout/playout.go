// Package playout 把收到的 RTP 包经抖动缓冲、解码后交给播放设备。
package playout

import (
	"log/slog"
	"sync"

	"nexcall/device/internal/jitter"
)

type Decoder interface {
	Decode(packet []byte) ([]int16, error)
	DecodeFEC(next []byte) ([]int16, error)
	DecodePLC() ([]int16, error)
}

type Stats struct {
	jitter.Stats
	FEC        uint64
	PLC        uint64
	DecodeErrs uint64
}

type Player struct {
	jb        *jitter.Buffer
	dec       Decoder
	frameSize int

	mu    sync.Mutex
	fifo  []int16
	stats Stats
	// 连续 PLC 次数过多时改为静音，避免长时间输出合成噪声
	plcRun int
}

func New(dec Decoder, frameSize, targetFrames, maxFrames int) *Player {
	return &Player{
		jb:        jitter.New(targetFrames, maxFrames),
		dec:       dec,
		frameSize: frameSize,
		fifo:      make([]int16, 0, frameSize*4),
	}
}

// Push 由 RTP 接收协程调用。payload 会被持有，调用方不能复用。
func (p *Player) Push(seq uint16, payload []byte) {
	p.jb.Push(seq, payload)
}

// Pull 由播放线程调用，填满 out。
func (p *Player) Pull(out []int16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.fifo) < len(out) {
		p.fifo = append(p.fifo, p.nextFrame()...)
	}
	n := copy(out, p.fifo)
	p.fifo = append(p.fifo[:0], p.fifo[n:]...)
}

const maxPLCRun = 5

func (p *Player) nextFrame() []int16 {
	r := p.jb.Pop()
	var (
		pcm []int16
		err error
	)
	switch r.Kind {
	case jitter.Frame:
		pcm, err = p.dec.Decode(r.Payload)
		p.plcRun = 0
	case jitter.Conceal:
		pcm, err = p.dec.DecodePLC()
		p.stats.PLC++
	case jitter.Lost:
		if r.FEC != nil {
			pcm, err = p.dec.DecodeFEC(r.FEC)
			p.stats.FEC++
		} else if p.plcRun < maxPLCRun {
			pcm, err = p.dec.DecodePLC()
			p.stats.PLC++
			p.plcRun++
		}
	}
	if err != nil {
		p.stats.DecodeErrs++
		if p.stats.DecodeErrs%50 == 1 {
			slog.Warn("opus decode failed", "err", err)
		}
		pcm = nil
	}
	if len(pcm) == 0 {
		return make([]int16, p.frameSize)
	}
	return pcm
}

func (p *Player) Stats() Stats {
	p.mu.Lock()
	s := p.stats
	p.mu.Unlock()
	s.Stats = p.jb.Stats()
	return s
}
