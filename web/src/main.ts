import './style.css'
import {
  listCalls,
  loadSession,
  login,
  me,
  saveSession,
  UnauthorizedError,
  type CallRecord,
  type DeviceStatus,
  type Session,
} from './api'
import { MediaCall } from './call'
import { Ringer } from './ringer'
import { SignalClient, type Message } from './signal'

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T

type Phase = 'dialing' | 'calling' | 'ringing' | 'incoming' | 'connecting' | 'active'

interface CurrentCall {
  id?: string
  peer: string
  phase: Phase
  muted: boolean
  startedAt?: number
  media?: MediaCall
}

const reasonText: Record<string, string> = {
  normal: '通话已结束',
  rejected: '对方已拒接',
  busy: '对方忙线中',
  timeout: '无人接听',
  offline: '对方不在线',
  disconnected: '对方已断开',
  answered_elsewhere: '已在其他页面接听',
  cancelled: '对方已取消',
}

const phaseText: Record<Phase, string> = {
  dialing: '正在呼叫…',
  calling: '正在呼叫…',
  ringing: '对方振铃中…',
  incoming: '来电',
  connecting: '正在接通…',
  active: '通话中',
}

let session: Session | null = null
let signal: SignalClient | null = null
let devices: DeviceStatus[] = []
let current: CurrentCall | null = null
let timerHandle: number | undefined
const ringer = new Ringer()

// ---------- 视图 ----------

function show(id: 'login-view' | 'main-view') {
  $('login-view').classList.toggle('hidden', id !== 'login-view')
  $('main-view').classList.toggle('hidden', id !== 'main-view')
}

let bannerTimer: number | undefined
function banner(text: string, persist = false) {
  const el = $('banner')
  el.textContent = text
  el.classList.toggle('hidden', !text)
  clearTimeout(bannerTimer)
  if (text && !persist) bannerTimer = window.setTimeout(() => el.classList.add('hidden'), 4000)
}

function peerLabel(addr: string) {
  if (addr === 'web:*') return '所有在线用户'
  const [kind, id] = addr.split(':')
  if (kind === 'device') return devices.find((d) => d.id === id)?.name ?? id
  return id ?? addr
}

function setConn(connected: boolean) {
  $('conn-dot').classList.toggle('on', connected)
  $('conn-text').textContent = connected ? '已连接' : '重连中…'
}

function renderDevices() {
  const list = $('device-list')
  list.replaceChildren()
  if (devices.length === 0) {
    const li = document.createElement('li')
    li.className = 'empty muted'
    li.textContent = '还没有登记设备，在服务器执行 call-server device add <ID> <名称>'
    list.append(li)
    return
  }
  for (const d of devices) {
    const li = document.createElement('li')
    li.className = 'device'
    const state = !d.online ? '离线' : d.busy ? '通话中' : '在线'
    li.innerHTML = `
      <div class="device-info">
        <span class="dot ${d.online ? (d.busy ? 'busy' : 'on') : ''}"></span>
        <div><div class="device-name"></div><div class="muted small"></div></div>
      </div>
      <button class="primary">呼叫</button>`
    li.querySelector('.device-name')!.textContent = d.name
    li.querySelector('.small')!.textContent = `${d.id} · ${state}`
    const btn = li.querySelector('button')!
    btn.disabled = !d.online || d.busy || current !== null
    btn.onclick = () => startCall(`device:${d.id}`)
    list.append(li)
  }
}

function fmtDuration(ms: number) {
  const s = Math.max(0, Math.floor(ms / 1000))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

async function refreshCalls() {
  if (!session) return
  try {
    renderCalls(await listCalls(session.token))
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
  }
}

function renderCalls(calls: CallRecord[]) {
  const tbody = $('call-list')
  tbody.replaceChildren()
  for (const c of calls) {
    const tr = document.createElement('tr')
    const answered = c.answeredAt ? new Date(c.answeredAt).getTime() : 0
    const ended = c.endedAt ? new Date(c.endedAt).getTime() : 0
    const cells = [
      new Date(c.startedAt).toLocaleString(),
      peerLabel(c.caller),
      peerLabel(c.callee),
      answered && ended ? fmtDuration(ended - answered) : '-',
      !c.endedAt ? '进行中' : answered ? '已接通' : reasonText[c.endReason ?? ''] ?? c.endReason ?? '',
    ]
    for (const text of cells) {
      const td = document.createElement('td')
      td.textContent = text
      tr.append(td)
    }
    tbody.append(tr)
  }
}

function renderCall() {
  const overlay = $('call-overlay')
  overlay.classList.toggle('hidden', !current)
  renderDevices()
  if (!current) return

  $('call-peer').textContent = peerLabel(current.peer)
  $('call-state').textContent = current.phase === 'incoming' ? `${phaseText.incoming}` : phaseText[current.phase]
  $('btn-accept').classList.toggle('hidden', current.phase !== 'incoming')
  $('btn-mute').classList.toggle('hidden', current.phase !== 'active')
  $('btn-mute').classList.toggle('active', current.muted)
  $('btn-mute').textContent = current.muted ? '取消静音' : '静音'
  $('btn-hangup').textContent = current.phase === 'incoming' ? '拒接' : current.phase === 'active' ? '挂断' : '取消'
  $('call-timer').classList.toggle('hidden', current.phase !== 'active')
  if (current.phase !== 'active') $('call-quality').textContent = ''
}

function startTimer() {
  clearInterval(timerHandle)
  timerHandle = window.setInterval(() => {
    if (current?.startedAt) $('call-timer').textContent = fmtDuration(Date.now() - current.startedAt)
  }, 500)
}

// ---------- 通话流程 ----------

function startCall(to: string) {
  if (current) return
  if (!window.isSecureContext) {
    banner('当前不是 HTTPS 页面，浏览器不允许使用麦克风')
    return
  }
  ringer.unlock()
  if (!signal?.send({ type: 'call', to })) {
    banner('信令未连接，请稍后再试')
    return
  }
  current = { peer: to, phase: 'dialing', muted: false }
  renderCall()
}

async function teardown(reason?: string) {
  const c = current
  current = null
  ringer.stop()
  clearInterval(timerHandle)
  $('btn-audio').classList.add('hidden')
  renderCall()
  if (c?.media) await c.media.close().catch(() => {})
  if (reason) banner(reasonText[reason] ?? reason)
  void refreshCalls()
}

function hangup() {
  if (!current) return
  if (current.id) {
    signal?.send({ type: current.phase === 'incoming' ? 'reject' : 'hangup', callId: current.id })
  }
  void teardown()
}

async function connectMedia(m: Message) {
  if (!current || !m.url || !m.token) return
  current.phase = 'connecting'
  renderCall()
  ringer.stop()

  const media = new MediaCall({
    onPeerLeft: () => {
      if (current?.media === media) hangup()
    },
    onDisconnected: () => {
      if (current?.media === media) {
        hangup()
        banner('媒体连接已断开')
      }
    },
    onAudioBlocked: (blocked) => $('btn-audio').classList.toggle('hidden', !blocked),
    onQuality: (text) => {
      $('call-quality').textContent = text
    },
  })
  current.media = media
  try {
    await media.start(m.url, m.token)
  } catch (e) {
    console.error(e)
    if (current?.media === media) {
      hangup()
      banner(e instanceof Error && e.name === 'NotAllowedError' ? '麦克风权限被拒绝' : '连接媒体服务器失败')
    }
    return
  }
  if (current?.media !== media) {
    await media.close()
    return
  }
  current.phase = 'active'
  current.startedAt = Date.now()
  startTimer()
  renderCall()
}

function onSignal(m: Message) {
  switch (m.type) {
    case 'registered':
      break
    case 'presence':
      devices = m.devices ?? []
      renderDevices()
      break
    case 'outgoing':
      if (current?.phase === 'dialing') {
        current.id = m.callId
        current.phase = 'calling'
        ringer.start('outgoing')
        renderCall()
      }
      break
    case 'ringing':
      if (current && current.id === m.callId) {
        current.phase = 'ringing'
        renderCall()
      }
      break
    case 'invite':
      if (current) {
        signal?.send({ type: 'reject', callId: m.callId })
        return
      }
      current = { id: m.callId, peer: m.from ?? '', phase: 'incoming', muted: false }
      signal?.send({ type: 'ringing', callId: m.callId })
      ringer.start('incoming')
      renderCall()
      if (document.hidden && 'Notification' in window && Notification.permission === 'granted') {
        new Notification('NexCall 来电', { body: peerLabel(current.peer) })
      }
      break
    case 'accepted':
      if (current && current.id === m.callId) void connectMedia(m)
      break
    case 'hangup':
      if (current && (current.id === m.callId || current.phase === 'dialing')) void teardown(m.reason)
      break
    case 'error':
      if (current?.phase === 'dialing') void teardown()
      banner(m.reason ?? '出错了')
      break
  }
}

// ---------- 登录 ----------

function startSession(s: Session) {
  session = s
  saveSession(s)
  $('user-name').textContent = s.username
  show('main-view')
  signal?.stop()
  signal = new SignalClient(s.token, onSignal, setConn, async () => {
    try {
      await me(s.token)
    } catch (e) {
      if (e instanceof UnauthorizedError) logout()
    }
  })
  signal.start()
  void refreshCalls()
  if ('Notification' in window && Notification.permission === 'default') {
    void Notification.requestPermission()
  }
}

function logout() {
  void teardown()
  signal?.stop()
  signal = null
  session = null
  saveSession(null)
  show('login-view')
}

$('login-form').addEventListener('submit', async (ev) => {
  ev.preventDefault()
  ringer.unlock()
  const form = new FormData(ev.target as HTMLFormElement)
  $('login-error').textContent = ''
  try {
    startSession(await login(String(form.get('username')), String(form.get('password'))))
  } catch (e) {
    $('login-error').textContent = e instanceof UnauthorizedError ? '用户名或密码错误' : String((e as Error).message)
  }
})

$('logout').onclick = logout
$('refresh-calls').onclick = () => void refreshCalls()
$('btn-hangup').onclick = hangup
$('btn-accept').onclick = () => {
  if (current?.phase !== 'incoming' || !current.id) return
  ringer.stop()
  signal?.send({ type: 'accept', callId: current.id })
  current.phase = 'connecting'
  renderCall()
}
$('btn-mute').onclick = async () => {
  if (!current?.media) return
  current.muted = !current.muted
  await current.media.setMuted(current.muted)
  renderCall()
}
$('btn-audio').onclick = async () => {
  await current?.media?.startAudio()
  $('btn-audio').classList.add('hidden')
}
window.addEventListener('beforeunload', () => {
  if (current?.id) signal?.send({ type: 'hangup', callId: current.id })
})

// ---------- 启动 ----------

if (!window.isSecureContext) {
  banner('当前页面不是 HTTPS，浏览器将禁止使用麦克风。请通过 https:// 访问。', true)
}

const saved = loadSession()
if (saved) {
  me(saved.token)
    .then(() => startSession(saved))
    .catch((e) => {
      if (e instanceof UnauthorizedError) logout()
      else startSession(saved)
    })
} else {
  show('login-view')
}
