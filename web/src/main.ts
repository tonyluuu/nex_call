import './style.css'
import {
  getCall,
  kick,
  listCalls,
  loadSession,
  login,
  me,
  saveSession,
  UnauthorizedError,
  type CallRecord,
  type DeviceStatus,
  type MemberStatus,
  type RoomStatus,
  type Session,
  type UserStatus,
} from './api'
import { MediaCall } from './call'
import { Ringer } from './ringer'
import { SignalClient, type Message } from './signal'

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T

interface CurrentCall {
  room: string
  phase: 'joining' | 'active'
  muted: boolean
  startedAt?: number
  media?: MediaCall
}

const reasonText: Record<string, string> = {
  normal: '已离开',
  busy: '对方忙线中',
  offline: '对方不在线',
  disconnected: '连接已断开',
  kicked: '已被管理员移出',
}

let session: Session | null = null
let signal: SignalClient | null = null
let devices: DeviceStatus[] = []
let users: UserStatus[] = []
let rooms: RoomStatus[] = []
let current: CurrentCall | null = null
let speaking = new Set<string>()
let notifyRoom = ''
let timerHandle: number | undefined
const ringer = new Ringer()

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

function isAdmin() {
  return session?.role === 'admin'
}

function peerLabel(addr: string) {
  const [kind, id] = addr.split(':')
  if (kind === 'device') return devices.find((d) => d.id === id)?.name ?? id ?? addr
  return id ?? addr
}

function roomLabel(room: RoomStatus | string) {
  const id = typeof room === 'string' ? room : room.id
  const deviceId = typeof room === 'string' ? rooms.find((r) => r.id === id)?.deviceId : room.deviceId
  if (deviceId) return devices.find((d) => d.id === deviceId)?.name ?? deviceId
  return '网页通话'
}

function setConn(connected: boolean) {
  $('conn-dot').classList.toggle('on', connected)
  $('conn-text').textContent = connected ? '已连接' : '重连中…'
}

function memberLine(members: MemberStatus[]) {
  if (!members?.length) return '空房间'
  return members.map((m) => peerLabel(m.address)).join('、')
}

function renderDevices() {
  const list = $('device-list')
  list.replaceChildren()
  if (devices.length === 0) {
    const li = document.createElement('li')
    li.className = 'empty muted'
    li.textContent = '还没有登记设备'
    list.append(li)
    return
  }
  for (const d of devices) {
    const li = document.createElement('li')
    li.className = 'device'
    const state = !d.online ? '离线' : d.busy ? '通话中' : '在房间待命'
    li.innerHTML = `
      <div class="device-info">
        <span class="dot ${d.online ? (d.busy ? 'busy' : 'on') : ''}"></span>
        <div><div class="device-name"></div><div class="muted small"></div></div>
      </div>
      <button class="primary">进入</button>`
    li.querySelector('.device-name')!.textContent = d.name
    li.querySelector('.small')!.textContent = `${d.id} · ${state}`
    const btn = li.querySelector('button')!
    btn.textContent = d.busy ? '加入' : '进入'
    btn.disabled = !d.online || current !== null
    btn.onclick = () => joinRoom(`device:${d.id}`)
    list.append(li)
  }
}

function renderUsers() {
  const list = $('user-list')
  list.replaceChildren()
  const others = users.filter((u) => u.id !== session?.username)
  if (others.length === 0) {
    const li = document.createElement('li')
    li.className = 'empty muted'
    li.textContent = '没有其他在线用户'
    list.append(li)
    return
  }
  for (const u of others) {
    const li = document.createElement('li')
    li.className = 'device'
    li.innerHTML = `
      <div class="device-info">
        <span class="dot ${u.busy ? 'busy' : 'on'}"></span>
        <div><div class="device-name"></div><div class="muted small"></div></div>
      </div>
      <button class="primary">呼叫</button>`
    li.querySelector('.device-name')!.textContent = u.id
    li.querySelector('.small')!.textContent = u.busy ? '通话中' : '在线'
    const btn = li.querySelector('button')!
    btn.disabled = u.busy || current !== null
    btn.onclick = () => callUser(u.id)
    list.append(li)
  }
}

function renderRooms(target: HTMLElement, withKick: boolean) {
  target.replaceChildren()
  const live = rooms.filter((r) => r.members?.some((m) => m.kind === 'web'))
  if (live.length === 0) {
    const li = document.createElement('li')
    li.className = 'empty muted'
    li.textContent = '当前没有进行中的通话'
    target.append(li)
    return
  }
  for (const room of live) {
    const li = document.createElement('li')
    li.className = 'device room-card'
    const info = document.createElement('div')
    info.className = 'device-info'
    info.innerHTML = `<div><div class="device-name"></div><div class="muted small"></div></div>`
    info.querySelector('.device-name')!.textContent = roomLabel(room)
    info.querySelector('.small')!.textContent = memberLine(room.members)
    li.append(info)
    const actions = document.createElement('div')
    actions.className = 'row-actions'
    if (current?.room !== room.id) {
      const join = document.createElement('button')
      join.className = 'primary'
      join.textContent = '加入'
      join.disabled = current !== null
      join.onclick = () => joinRoom(room.id)
      actions.append(join)
    }
    if (withKick) {
      for (const m of room.members.filter((x) => x.kind === 'web')) {
        const kickBtn = document.createElement('button')
        kickBtn.textContent = `移出 ${peerLabel(m.address)}`
        kickBtn.onclick = () => void doKick(room.id, m.address)
        actions.append(kickBtn)
      }
    }
    li.append(actions)
    target.append(li)
  }
}

function fmtDuration(ms: number) {
  const s = Math.max(0, Math.floor(ms / 1000))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

async function refreshCalls() {
  if (!session) return
  try {
    const calls = await listCalls(session.token)
    renderCalls($('call-list'), calls, false)
    if (isAdmin()) renderCalls($('admin-call-list'), calls, true)
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
  }
}

function renderCalls(tbody: HTMLElement, calls: CallRecord[], expandable: boolean) {
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
      !c.endedAt ? '进行中' : reasonText[c.endReason ?? ''] ?? (answered ? '已结束' : c.endReason ?? ''),
    ]
    for (const text of cells) {
      const td = document.createElement('td')
      td.textContent = text
      tr.append(td)
    }
    tbody.append(tr)
    if (!expandable || !session) continue
    tr.classList.add('clickable')
    tr.onclick = async () => {
      const next = tr.nextElementSibling
      if (next?.classList.contains('detail')) {
        next.remove()
        return
      }
      try {
        const detail = await getCall(session!.token, c.id)
        const row = document.createElement('tr')
        row.className = 'detail'
        const td = document.createElement('td')
        td.colSpan = 5
        td.textContent = (detail.participants ?? [])
          .map((p) => `${peerLabel(p.identity)}${p.leftAt ? '' : '（在场）'}${p.reason === 'kicked' ? ' 被移出' : ''}`)
          .join(' · ') || '没有参与者记录'
        row.append(td)
        tr.after(row)
      } catch (e) {
        banner(e instanceof Error ? e.message : '加载失败')
      }
    }
  }
}

function renderCall() {
  $('call-overlay').classList.toggle('hidden', !current)
  renderDevices()
  renderUsers()
  renderRooms($('room-list'), false)
  if (isAdmin()) renderRooms($('admin-rooms'), true)
  if (!current) return
  const room = rooms.find((r) => r.id === current!.room)
  $('call-peer').textContent = room ? roomLabel(room) : '正在进入…'
  $('call-state').textContent = current.phase === 'active' ? '通话中' : '正在进入…'
  $('btn-mute').classList.toggle('hidden', current.phase !== 'active')
  $('btn-mute').classList.toggle('active', current.muted)
  $('btn-mute').textContent = current.muted ? '取消静音' : '静音'
  $('btn-hangup').textContent = '离开'
  $('call-timer').classList.toggle('hidden', current.phase !== 'active')
  const members = $('call-members')
  members.replaceChildren()
  for (const m of room?.members ?? []) {
    const li = document.createElement('li')
    li.textContent = peerLabel(m.address)
    if (speaking.has(m.address)) li.classList.add('speaking')
    members.append(li)
  }
}

function startTimer() {
  clearInterval(timerHandle)
  timerHandle = window.setInterval(() => {
    if (current?.startedAt) $('call-timer').textContent = fmtDuration(Date.now() - current.startedAt)
  }, 500)
}

function joinRoom(room: string) {
  if (current || !room) return
  if (!window.isSecureContext) {
    banner('当前不是 HTTPS 页面，浏览器不允许使用麦克风')
    return
  }
  ringer.unlock()
  if (!signal?.send({ type: 'join', room })) {
    banner('信令未连接，请稍后再试')
    return
  }
  hideNotify()
  current = { room, phase: 'joining', muted: false }
  renderCall()
}

function callUser(id: string) {
  if (current) return
  if (!window.isSecureContext) {
    banner('当前不是 HTTPS 页面，浏览器不允许使用麦克风')
    return
  }
  ringer.unlock()
  if (!signal?.send({ type: 'call', to: `web:${id}` })) {
    banner('信令未连接，请稍后再试')
    return
  }
  current = { room: '', phase: 'joining', muted: false }
  renderCall()
}

async function teardown(reason?: string) {
  const c = current
  current = null
  speaking = new Set()
  clearInterval(timerHandle)
  $('btn-audio').classList.add('hidden')
  renderCall()
  if (c?.media) await c.media.close().catch(() => {})
  if (reason) banner(reasonText[reason] ?? reason)
  void refreshCalls()
}

function hangup() {
  if (!current) return
  if (current.room) signal?.send({ type: 'leave', room: current.room })
  void teardown()
}

async function connectMedia(m: Message) {
  if (!current || !m.url || !m.token) return
  current.room = m.room ?? current.room
  current.phase = 'joining'
  renderCall()

  const media = new MediaCall({
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
    onParticipants: (list) => {
      speaking = new Set(list.filter((p) => p.speaking).map((p) => p.identity))
      renderCall()
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

function showNotify(from: string, room: string) {
  notifyRoom = room
  $('notify-text').textContent = `${peerLabel(from)} 在叫你`
  $('notify-bar').classList.remove('hidden')
}

function hideNotify() {
  notifyRoom = ''
  $('notify-bar').classList.add('hidden')
}

async function doKick(room: string, identity: string) {
  if (!session) return
  try {
    await kick(session.token, room, identity)
  } catch (e) {
    banner(e instanceof Error ? e.message : '移出失败')
  }
}

function onSignal(m: Message) {
  switch (m.type) {
    case 'presence':
      devices = m.devices ?? []
      users = m.users ?? []
      rooms = m.rooms ?? []
      renderCall()
      break
    case 'joined':
      if (!current) current = { room: m.room ?? '', phase: 'joining', muted: false }
      void connectMedia(m)
      break
    case 'invite':
      banner(`${peerLabel(m.from ?? '')} 拉你进入通话`)
      if (!current && m.room) joinRoom(m.room)
      break
    case 'notify':
      if (m.room) showNotify(m.from ?? '', m.room)
      break
    case 'left':
      if (current && current.room === m.room) void teardown(m.reason)
      break
    case 'kicked':
      if (current) void teardown('kicked')
      break
    case 'error':
      if (current?.phase === 'joining') void teardown()
      banner(reasonText[m.reason ?? ''] ?? m.reason ?? '出错了')
      break
  }
}

function startSession(s: Session) {
  session = s
  saveSession(s)
  $('user-name').textContent = s.role === 'admin' ? `${s.username} · 管理员` : s.username
  $('tab-admin').classList.toggle('hidden', s.role !== 'admin')
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

function showTab(admin: boolean) {
  $('call-panel').classList.toggle('hidden', admin)
  $('admin-panel').classList.toggle('hidden', !admin)
  $('tab-call').classList.toggle('on', !admin)
  $('tab-admin').classList.toggle('on', admin)
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
$('refresh-admin-calls').onclick = () => void refreshCalls()
$('tab-call').onclick = () => showTab(false)
$('tab-admin').onclick = () => showTab(true)
$('btn-hangup').onclick = hangup
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
$('notify-join').onclick = () => {
  if (current) {
    banner('请先离开当前通话')
    return
  }
  const room = notifyRoom
  hideNotify()
  joinRoom(room)
}
$('notify-dismiss').onclick = hideNotify
window.addEventListener('beforeunload', () => {
  if (current?.room) signal?.send({ type: 'leave', room: current.room })
})

if (!window.isSecureContext) {
  banner('当前页面不是 HTTPS，浏览器将禁止使用麦克风。请通过 https:// 访问。', true)
}

const saved = loadSession()
if (saved) {
  me(saved.token)
    .then((info) => startSession({ ...saved, username: info.username, livekitUrl: info.livekitUrl, role: info.role || 'user' }))
    .catch((e) => {
      if (e instanceof UnauthorizedError) logout()
      else startSession({ ...saved, role: saved.role || 'user' })
    })
} else {
  show('login-view')
}
