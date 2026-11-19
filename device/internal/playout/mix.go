package playout

// Mix 把多路 PCM 相加写入 dst，结果饱和到 int16。
func Mix(dst []int16, srcs [][]int16) {
	for i := range dst {
		var sum int32
		for _, src := range srcs {
			if i < len(src) {
				sum += int32(src[i])
			}
		}
		switch {
		case sum > 32767:
			sum = 32767
		case sum < -32768:
			sum = -32768
		}
		dst[i] = int16(sum)
	}
}
