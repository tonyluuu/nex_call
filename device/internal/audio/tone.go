package audio

import "math"

// Tone 生成正弦提示音，可按节奏开关（用于铃声和无麦克风时的测试音）。
type Tone struct {
	rate    float64
	freqs   []float64
	amp     float64
	onSamp  int
	offSamp int
	pos     int
	phase   []float64
}

// NewTone onMs/offMs 为 0 表示持续发声。
func NewTone(sampleRate int, amp float64, onMs, offMs int, freqs ...float64) *Tone {
	return &Tone{
		rate:    float64(sampleRate),
		freqs:   freqs,
		amp:     amp,
		onSamp:  sampleRate * onMs / 1000,
		offSamp: sampleRate * offMs / 1000,
		phase:   make([]float64, len(freqs)),
	}
}

// Ringtone 类似国内座机回铃：双音 1 秒响、2 秒停。
func Ringtone(sampleRate int) *Tone {
	return NewTone(sampleRate, 0.25, 1000, 2000, 440, 480)
}

func (t *Tone) Fill(out []int16) {
	period := t.onSamp + t.offSamp
	for i := range out {
		on := period == 0 || t.pos%period < t.onSamp
		var v float64
		for k, f := range t.freqs {
			if on {
				v += math.Sin(t.phase[k])
			}
			t.phase[k] += 2 * math.Pi * f / t.rate
			if t.phase[k] > 2*math.Pi {
				t.phase[k] -= 2 * math.Pi
			}
		}
		if len(t.freqs) > 0 {
			v /= float64(len(t.freqs))
		}
		out[i] = int16(v * t.amp * math.MaxInt16)
		t.pos++
	}
}
