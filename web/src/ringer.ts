// 用 WebAudio 合成铃声，避免额外的音频文件。
export class Ringer {
  private ctx: AudioContext | null = null
  private timer: number | undefined
  private nodes: OscillatorNode[] = []

  // 需在用户手势（点击）中调用一次，解除浏览器自动播放限制
  unlock() {
    if (!this.ctx) this.ctx = new AudioContext()
    void this.ctx.resume()
  }

  start(kind: 'incoming' | 'outgoing') {
    this.stop()
    this.unlock()
    const [on, period, freqs] = kind === 'incoming' ? [1000, 3000, [440, 480]] : [1000, 4000, [450]]
    const beep = () => this.beep(freqs, on)
    beep()
    this.timer = window.setInterval(beep, period)
  }

  stop() {
    clearInterval(this.timer)
    this.timer = undefined
    for (const n of this.nodes) {
      try {
        n.stop()
      } catch {
        /* already stopped */
      }
    }
    this.nodes = []
  }

  private beep(freqs: number[], ms: number) {
    const ctx = this.ctx
    if (!ctx) return
    const gain = ctx.createGain()
    gain.gain.value = 0.08
    gain.connect(ctx.destination)
    const t = ctx.currentTime
    for (const f of freqs) {
      const osc = ctx.createOscillator()
      osc.frequency.value = f
      osc.connect(gain)
      osc.start(t)
      osc.stop(t + ms / 1000)
      this.nodes.push(osc)
      osc.onended = () => {
        this.nodes = this.nodes.filter((n) => n !== osc)
      }
    }
  }
}
