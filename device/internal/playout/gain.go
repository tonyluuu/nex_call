package playout

import "math"

// 低于该比例的样本线性放大，超过后用 tanh 平滑压到满幅以内，避免硬削波破音。
const gainKnee = 0.7

// ApplyGain 把 buf 原地乘以 gain。gain 为 1 或不大于 0 时不处理。
func ApplyGain(buf []int16, gain float64) {
	if gain == 1 || gain <= 0 {
		return
	}
	for i, s := range buf {
		x := float64(s) / 32768 * gain
		a := math.Abs(x)
		if a > gainKnee {
			a = gainKnee + (1-gainKnee)*math.Tanh((a-gainKnee)/(1-gainKnee))
			x = math.Copysign(a, x)
		}
		v := math.Round(x * 32768)
		switch {
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		buf[i] = int16(v)
	}
}
