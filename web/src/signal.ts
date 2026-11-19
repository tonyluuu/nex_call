import { apiPath, type DeviceStatus, type RoomStatus, type UserStatus } from './api'

export interface Message {
  type: string
  callId?: string
  room?: string
  token?: string
  url?: string
  from?: string
  to?: string
  peer?: string
  reason?: string
  self?: string
  devices?: DeviceStatus[]
  users?: UserStatus[]
  rooms?: RoomStatus[]
}

type Listener = (m: Message) => void
type StateListener = (connected: boolean) => void

// SignalClient 维护到 call-server 的 WebSocket，断线后指数退避重连。
export class SignalClient {
  private ws: WebSocket | null = null
  private retry = 0
  private stopped = false
  private pingTimer: number | undefined
  private reconnectTimer: number | undefined

  constructor(
    private token: string,
    private onMessage: Listener,
    private onState: StateListener,
    private onUnauthorized: () => void,
  ) {}

  start() {
    this.stopped = false
    this.connect()
  }

  stop() {
    this.stopped = true
    clearTimeout(this.reconnectTimer)
    clearInterval(this.pingTimer)
    this.ws?.close()
    this.ws = null
  }

  get connected() {
    return this.ws?.readyState === WebSocket.OPEN
  }

  send(m: Message): boolean {
    if (!this.connected) return false
    this.ws!.send(JSON.stringify(m))
    return true
  }

  private connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(`${proto}//${location.host}${apiPath('/ws/web')}?token=${encodeURIComponent(this.token)}`)
    this.ws = ws
    let opened = false

    ws.onopen = () => {
      opened = true
      this.retry = 0
      this.onState(true)
      clearInterval(this.pingTimer)
      this.pingTimer = window.setInterval(() => this.send({ type: 'ping' }), 20_000)
    }
    ws.onmessage = (ev) => {
      try {
        this.onMessage(JSON.parse(ev.data) as Message)
      } catch (e) {
        console.warn('bad signal message', e)
      }
    }
    ws.onclose = (ev) => {
      clearInterval(this.pingTimer)
      if (this.ws === ws) this.ws = null
      this.onState(false)
      if (this.stopped) return
      // 握手被拒（令牌失效）和网络故障在浏览器里都是 1006，交给上层用 /api/me 确认
      if (!opened && ev.code === 1006 && this.retry >= 2) {
        this.onUnauthorized()
      }
      const delay = Math.min(30_000, 1000 * 2 ** this.retry++)
      this.reconnectTimer = window.setTimeout(() => this.connect(), delay)
    }
  }
}
