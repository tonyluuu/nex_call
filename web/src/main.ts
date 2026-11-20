import './style.css'
import {
  createUser,
  deleteUser,
  getCall,
  kick,
  listCalls,
  listUsers,
  loadSession,
  login,
  me,
  saveSession,
  setDeviceVolume,
  UnauthorizedError,
  type CallRecord,
  type DeviceStatus,
  type MemberStatus,
  type RoomStatus,
  type Session,
  type UserAccount,
  type UserStatus,
} from './api'
import { MediaCall } from './call'
import { Ringer } from './ringer'
import { SignalClient, type Message } from './signal'

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T

interface CurrentCall {
  room: string
  phase: 'ringing' | 'joining' | 'active'
  muted: boolean
  minimized: boolean
  peerName?: string
  pendingJoin?: Message
  startedAt?: number
  media?: MediaCall
}

const reasonText: Record<string, string> = {
  normal: '已离开',
  busy: '对方忙线中',
  offline: '对方不在线',
  disconnected: '连接已断开',
  kicked: '已被管理员移出',
  peer_left: '对方已离开，通话结束',
  timeout: '无人加入，通话已结束',
  declined: '对方已挂断',
  replaced: '账号已在其他地方登录',
  deleted: '账号已被删除',
}

const apiErrorText: Record<string, string> = {
  'user exists': '用户名已存在',
  'invalid username': '用户名不能为空，且不能包含空格或冒号',
  'password must be at least 8 characters': '密码至少 8 位',
  'cannot delete yourself': '不能删除当前登录的账号',
  'cannot delete admin': '不能删除管理员',
  'device offline': '设备不在线',
  'bad request': '参数不正确',
}

let session: Session | null = null
let signal: SignalClient | null = null
let devices: DeviceStatus[] = []
let users: UserStatus[] = []
let rooms: RoomStatus[] = []
let current: CurrentCall | null = null
let speaking = new Set<string>()
let notifyRoom = ''
let incoming: { room: string; from: string } | null = null
let timerHandle: number | undefined
let volumeFor = ''
let volumeBusyUntil = 0
let volumeTimer: number | undefined
let volumePending: { speaker?: number; mic?: number } = {}
const ringer = new Ringer()

function explain(e: unknown) {
  const msg = e instanceof Error ? e.message : '操作失败'
  return apiErrorText[msg] ?? msg
}

function show(id: 'login-view' | 'main-view') {
  $('login-view').classList.toggle('hidden', id !== 'login-view')
  $('main-view').classList.toggle('hidden', id !== 'main-view')
  document.body.classList.toggle('login-page', id === 'login-view')
}

function banner(text: string, persist = false) {
  const el = $('banner')
  if (!text) {
    el.replaceChildren()
    el.classList.add('hidden')
    return
  }
  el.classList.remove('hidden')
  const item = document.createElement('div')
  item.className = 'banner-item'
  item.textContent = text
  el.append(item)
  if (!persist) {
    window.setTimeout(() => {
      item.remove()
      if (!el.childElementCount) el.classList.add('hidden')
    }, 4000)
  }
}

function isAdmin() {
  return session?.role === 'admin'
}

function sameRoom(a?: string, b?: string) {
  if (!a || !b) return false
  if (a === b) return true
  const norm = (id: string) => (id.startsWith('device:') ? `device-${id.slice(7)}` : id)
  return norm(a) === norm(b)
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
  $('conn-text').textContent = connected ? '在线' : '重连中…'
}

function memberLine(members: MemberStatus[]) {
  if (!members?.length) return '空房间'
  return members.map((m) => peerLabel(m.address)).join('、')
}

function fmtVol(v?: number) {
  return v == null ? '—' : `${v}%`
}

function snap5(v: number) {
  return Math.max(0, Math.min(200, Math.round(v / 5) * 5))
}

function showVolume(textId: string, v?: number) {
  $(textId).textContent = v == null ? '—' : `${v}%`
  $(textId.replace(/-text$/, '-gain')).classList.toggle('on', v != null && v > 100)
}

function renderAdminDevices() {
  const list = $('admin-devices')
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
      </div>`
    li.querySelector('.device-name')!.textContent = d.name
    li.querySelector('.small')!.textContent = `${d.id} · ${state} · 喇叭 ${fmtVol(d.speaker)} · 麦克风 ${fmtVol(d.mic)}`
    const btn = document.createElement('button')
    btn.textContent = `${d.name}音量设置`
    btn.disabled = !d.online
    btn.onclick = () => openVolume(d.id)
    li.append(btn)
    list.append(li)
  }
}

function setSlider(id: string, textId: string, value?: number) {
  const input = $<HTMLInputElement>(id)
  const known = value != null
  input.disabled = !known
  if (!known) {
    showVolume(textId)
    return
  }
  const v = snap5(value)
  input.value = String(v)
  showVolume(textId, v)
}

function openVolume(id: string) {
  const d = devices.find((x) => x.id === id)
  if (!d?.online) return
  volumeFor = id
  volumePending = {}
  volumeBusyUntil = 0
  $('volume-title').textContent = d.name
  setSlider('volume-speaker', 'volume-speaker-text', d.speaker)
  setSlider('volume-mic', 'volume-mic-text', d.mic)
  $('volume-dialog').classList.remove('hidden')
}

function closeVolume() {
  volumeFor = ''
  volumePending = {}
  clearTimeout(volumeTimer)
  $('volume-dialog').classList.add('hidden')
}

function syncVolumeDialog() {
  if (!volumeFor) return
  const d = devices.find((x) => x.id === volumeFor)
  if (!d?.online) {
    closeVolume()
    banner('设备已离线')
    return
  }
  $('volume-title').textContent = d.name
  if (Date.now() < volumeBusyUntil) return
  setSlider('volume-speaker', 'volume-speaker-text', d.speaker)
  setSlider('volume-mic', 'volume-mic-text', d.mic)
}

function queueVolume(part: { speaker?: number; mic?: number }) {
  volumeBusyUntil = Date.now() + 1500
  volumePending = { ...volumePending, ...part }
  if (volumeTimer != null) return
  volumeTimer = window.setTimeout(() => void flushVolume(), 150)
}

async function flushVolume() {
  volumeTimer = undefined
  if (!session || !volumeFor) return
  const body = volumePending
  volumePending = {}
  if (body.speaker == null && body.mic == null) return
  try {
    await setDeviceVolume(session.token, volumeFor, body)
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
    else banner(explain(e))
  }
}

let actionUser = ''
let accountCache: UserAccount[] | null = null

function renderAccounts(accounts: UserAccount[]) {
  accountCache = accounts
  const list = $('admin-users')
  list.replaceChildren()
  if (accounts.length === 0) {
    const tr = document.createElement('tr')
    const td = document.createElement('td')
    td.colSpan = 4
    td.className = 'muted'
    td.textContent = '还没有用户'
    tr.append(td)
    list.append(tr)
    return
  }
  const online = new Set(users.map((u) => u.id))
  for (const u of accounts) {
    const tr = document.createElement('tr')
    const created = new Date(u.createdAt)
    const when = Number.isNaN(created.getTime()) ? '' : `${fmtDate(created)} ${fmtClock(created)}`
    const name = document.createElement('td')
    const nameWrap = document.createElement('span')
    nameWrap.className = 'account-name'
    const label = document.createElement('span')
    label.textContent = u.username
    const dot = document.createElement('span')
    const isOnline = online.has(u.username)
    dot.className = `dot${isOnline ? ' on' : ''}`
    dot.title = isOnline ? '在线' : '离线'
    nameWrap.append(label, dot)
    name.append(nameWrap)
    tr.append(name)
    for (const text of [u.role === 'admin' ? '管理员' : '普通用户', when]) {
      const td = document.createElement('td')
      td.textContent = text
      tr.append(td)
    }
    const op = document.createElement('td')
    const btn = document.createElement('button')
    btn.textContent = '操作'
    const canDelete = u.role !== 'admin' && u.username !== session?.username
    btn.onclick = () => openUserAction(u.username, canDelete)
    op.append(btn)
    tr.append(op)
    list.append(tr)
  }
}

async function refreshUsers() {
  if (!session || !isAdmin()) return
  try {
    renderAccounts(await listUsers(session.token))
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
    else banner(explain(e))
  }
}

function openUserAction(username: string, canDelete: boolean) {
  actionUser = username
  $('user-action-title').textContent = username
  const del = $<HTMLButtonElement>('user-action-delete')
  del.disabled = !canDelete
  del.title = canDelete ? '' : '不能删除管理员或当前登录用户'
  $('user-action-dialog').classList.remove('hidden')
}

function closeUserAction() {
  $('user-action-dialog').classList.add('hidden')
}

async function removeUser(username: string) {
  if (!session) return
  $('user-confirm-error').textContent = ''
  try {
    await deleteUser(session.token, username)
    $('user-confirm-dialog').classList.add('hidden')
    await refreshUsers()
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
    else $('user-confirm-error').textContent = explain(e)
  }
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
      <div class="device-actions"></div>`
    li.querySelector('.device-name')!.textContent = d.name
    const volume = d.online && (d.speaker != null || d.mic != null) ? ` · 喇叭 ${fmtVol(d.speaker)} · 麦克风 ${fmtVol(d.mic)}` : ''
    li.querySelector('.small')!.textContent = `${d.id} · ${state}${volume}`
    const actions = li.querySelector('.device-actions')!
    if (isAdmin()) {
      const vol = document.createElement('button')
      vol.textContent = `${d.name}音量设置`
      vol.disabled = !d.online
      vol.onclick = () => openVolume(d.id)
      actions.append(vol)
    }
    const btn = document.createElement('button')
    btn.className = 'primary'
    btn.textContent = d.busy ? '加入' : '进入'
    btn.disabled = !d.online || current !== null
    btn.onclick = () => joinRoom(`device:${d.id}`)
    actions.append(btn)
    list.append(li)
  }
}

function userAvatar(id: string, busy: boolean, onClick: () => void, disabled = false) {
  const btn = document.createElement('button')
  btn.className = 'avatar'
  btn.type = 'button'
  btn.title = busy ? `${id} · 通话中` : id
  btn.disabled = disabled
  const name = document.createElement('span')
  name.className = 'avatar-name'
  name.textContent = id
  const dot = document.createElement('span')
  dot.className = `dot ${busy ? 'busy' : 'on'}`
  btn.append(name, dot)
  btn.onclick = onClick
  return btn
}

function renderUsers() {
  const list = $('user-list')
  list.replaceChildren()
  const me = session?.username
  if (me) {
    const self = users.find((u) => u.id === me)
    const li = document.createElement('li')
    li.className = 'user-self'
    const label = document.createElement('span')
    label.className = 'self-label'
    label.textContent = '自己'
    const btn = userAvatar(me, Boolean(self?.busy || current), () => banner('不能与自己通话'))
    btn.title = '不能与自己通话'
    li.append(label, btn)
    list.append(li)
    const split = document.createElement('li')
    split.className = 'user-split'
    split.setAttribute('aria-hidden', 'true')
    list.append(split)
  }
  const others = users.filter((u) => u.id !== me)
  const group = document.createElement('li')
  group.className = 'user-others'
  const caption = document.createElement('span')
  caption.className = 'self-label'
  caption.textContent = '他人'
  const row = document.createElement('div')
  row.className = 'user-others-row'
  if (others.length === 0) {
    const empty = document.createElement('span')
    empty.className = 'empty muted'
    empty.textContent = '没有其他在线用户'
    row.append(empty)
  } else {
    for (const u of others) {
      row.append(userAvatar(u.id, u.busy, () => callUser(u.id), u.busy || current !== null))
    }
  }
  group.append(caption, row)
  list.append(group)
}

function renderRooms(target: HTMLElement, withKick: boolean) {
  target.replaceChildren()
  const live = rooms.filter((r) => {
    const webs = r.members?.filter((m) => m.kind === 'web') ?? []
    if (webs.length === 0) return false
    if (r.deviceId) return true
    return (r.members?.length ?? 0) >= 2
  })
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

// 服务端只支持 limit，一次取最近 MAX_CALLS 条，在页面上分页
const MAX_CALLS = 500
let callCache: CallRecord[] = []
const callPage: Record<string, number> = { 'call-list': 0, 'admin-call-list': 0 }
const pageSize: Record<string, number> = { 'call-list': 10, 'admin-call-list': 10 }

async function refreshCalls() {
  if (!session) return
  try {
    callCache = await listCalls(session.token, MAX_CALLS)
    renderCallPage('call-list', false)
    if (isAdmin()) renderCallPage('admin-call-list', true)
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
  }
}

function renderCallPage(tbodyId: string, expandable: boolean) {
  const size = pageSize[tbodyId]
  const pages = Math.max(1, Math.ceil(callCache.length / size))
  const page = Math.min(Math.max(callPage[tbodyId] ?? 0, 0), pages - 1)
  callPage[tbodyId] = page
  renderCalls($(tbodyId), callCache.slice(page * size, (page + 1) * size), expandable)

  const pager = $(`${tbodyId}-pager`)
  pager.classList.toggle('hidden', callCache.length === 0)
  pager.replaceChildren()
  if (callCache.length === 0) return
  const go = (p: number) => {
    callPage[tbodyId] = p
    renderCallPage(tbodyId, expandable)
  }
  const prev = document.createElement('button')
  prev.textContent = '上一页'
  prev.disabled = page === 0
  prev.onclick = () => go(page - 1)
  const next = document.createElement('button')
  next.textContent = '下一页'
  next.disabled = page >= pages - 1
  next.onclick = () => go(page + 1)
  const total = document.createElement('span')
  total.className = 'muted small'
  total.textContent = `共 ${callCache.length} 条`
  pager.append(prev, next, total)

  const jump = document.createElement('label')
  jump.className = 'pager-field'
  const jumpInput = document.createElement('input')
  jumpInput.type = 'number'
  jumpInput.min = '1'
  jumpInput.max = String(pages)
  jumpInput.value = String(page + 1)
  jumpInput.setAttribute('aria-label', '页码')
  jumpInput.onchange = () => {
    const n = Math.round(Number(jumpInput.value))
    if (n >= 1 && n <= pages) go(n - 1)
  }
  jump.append('第 ', jumpInput, ` / ${pages} 页`)

  const sizeField = document.createElement('label')
  sizeField.className = 'pager-field'
  const sizeInput = document.createElement('input')
  sizeInput.type = 'number'
  sizeInput.min = '1'
  sizeInput.max = '100'
  sizeInput.value = String(size)
  sizeInput.setAttribute('aria-label', '每页条数')
  sizeInput.onchange = () => {
    const n = Math.round(Number(sizeInput.value))
    pageSize[tbodyId] = Math.max(1, Math.min(100, Number.isFinite(n) && n > 0 ? n : size))
    callPage[tbodyId] = 0
    renderCallPage(tbodyId, expandable)
  }
  sizeField.append('每页 ', sizeInput, ' 条')
  pager.append(jump, sizeField)
}

function fmtDate(d: Date) {
  return d.toLocaleDateString('zh-CN')
}

function fmtClock(d: Date) {
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

// 结束时间和开始在同一天时只写时分秒
function callTimeRange(c: CallRecord) {
  const start = new Date(c.startedAt)
  const head = `${fmtDate(start)} ${fmtClock(start)}`
  if (!c.endedAt) return `${head} - 进行中`
  const end = new Date(c.endedAt)
  const day = fmtDate(end) === fmtDate(start) ? '' : `${fmtDate(end)} `
  return `${head} - ${day}${fmtClock(end)}`
}

function callResult(c: CallRecord) {
  if (!c.endedAt) return '进行中'
  if (!c.answeredAt && c.endReason === 'declined') return '对方拒绝'
  if (!c.answeredAt && c.endReason === 'normal') return '主动挂断'
  return reasonText[c.endReason ?? ''] ?? (c.answeredAt ? '已结束' : c.endReason ?? '')
}

function renderCalls(tbody: HTMLElement, calls: CallRecord[], expandable: boolean) {
  tbody.replaceChildren()
  for (const c of calls) {
    const tr = document.createElement('tr')
    const answered = c.answeredAt ? new Date(c.answeredAt).getTime() : 0
    const ended = c.endedAt ? new Date(c.endedAt).getTime() : 0
    const cells = [
      ['时间', callTimeRange(c)],
      ['主叫', peerLabel(c.caller)],
      ['被叫', peerLabel(c.callee)],
      ['时长', answered && ended ? fmtDuration(ended - answered) : '-'],
      ['结果', callResult(c)],
    ]
    for (const [label, text] of cells) {
      const td = document.createElement('td')
      td.dataset.label = label
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
  $('call-overlay').classList.toggle('hidden', !current || current.minimized)
  $('call-mini').classList.toggle('hidden', !current?.minimized)
  $('btn-minimize').classList.toggle('hidden', !isAdmin())
  renderDevices()
  renderUsers()
  renderRooms($('room-list'), false)
  if (isAdmin()) {
    renderAdminDevices()
    renderRooms($('admin-rooms'), true)
    syncVolumeDialog()
    if (accountCache) renderAccounts(accountCache)
  }
  if (!current) return
  const room = rooms.find((r) => r.id === current!.room)
  const title = current.phase === 'ringing' && current.peerName ? `正在呼叫 ${current.peerName}` : room ? roomLabel(room) : '正在进入…'
  $('call-peer').textContent = title
  $('mini-peer').textContent = title
  $('call-state').textContent = current.phase === 'active' ? '通话中' : current.phase === 'ringing' ? '等待对方接听' : '正在进入…'
  $('btn-mute').classList.toggle('hidden', current.phase !== 'active')
  $('btn-mute').classList.toggle('active', current.muted)
  $('btn-mute').textContent = current.muted ? '取消静音' : '静音'
  $('mini-mute').textContent = current.muted ? '取消静音' : '静音'
  $('mini-mute').classList.toggle('active', current.muted)
  $('btn-hangup').textContent = current.phase === 'ringing' ? '挂断' : '离开'
  $('call-timer').classList.toggle('hidden', current.phase !== 'active')
  const members = $('call-members')
  members.replaceChildren()
  const self = session ? `web:${session.username}` : ''
  for (const m of room?.members ?? []) {
    if (m.address === self) continue
    const li = document.createElement('li')
    li.textContent = peerLabel(m.address)
    if (speaking.has(m.address)) li.classList.add('speaking')
    members.append(li)
  }
  members.classList.toggle('hidden', members.childElementCount === 0)
}

function startTimer() {
  clearInterval(timerHandle)
  timerHandle = window.setInterval(() => {
    if (!current?.startedAt) return
    const text = fmtDuration(Date.now() - current.startedAt)
    $('call-timer').textContent = text
    $('mini-timer').textContent = text
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
  current = { room, phase: 'joining', muted: false, minimized: false }
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
  current = { room: '', phase: 'ringing', muted: false, minimized: false, peerName: id }
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

function showIncoming(from: string, room: string) {
  incoming = { room, from }
  $('incoming-text').textContent = `${peerLabel(from)} 来电`
  $('incoming-call').classList.remove('hidden')
}

function hideIncoming() {
  incoming = null
  $('incoming-call').classList.add('hidden')
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
      if (m.pending) {
        if (!current) {
          if (m.room) signal?.send({ type: 'leave', room: m.room })
          break
        }
        current.room = m.room ?? current.room
        current.phase = 'ringing'
        current.pendingJoin = m
        renderCall()
        break
      }
      if (!current || (m.room && current.room && !sameRoom(current.room, m.room))) {
        if (m.room) signal?.send({ type: 'leave', room: m.room })
        break
      }
      void connectMedia(m)
      break
    case 'answered':
      if (current?.pendingJoin && (!m.room || m.room === current.pendingJoin.room)) void connectMedia(current.pendingJoin)
      break
    case 'invite':
      if (!m.room) break
      if (current) {
        signal?.send({ type: 'reject', room: m.room })
        break
      }
      showIncoming(m.from ?? '', m.room)
      break
    case 'notify':
      if (m.room) showNotify(m.from ?? '', m.room)
      break
    case 'left':
      if (incoming && m.room === incoming.room) hideIncoming()
      if (current && (!current.room || sameRoom(current.room, m.room))) void teardown(m.reason)
      break
    case 'kicked':
      if (current) void teardown('kicked')
      break
    case 'session_ended':
      logout()
      banner(reasonText[m.reason ?? ''] ?? '登录已失效', true)
      break
    case 'error':
      if (current?.phase === 'joining' || current?.phase === 'ringing') void teardown()
      banner(reasonText[m.reason ?? ''] ?? m.reason ?? '出错了')
      break
  }
}

function startSession(s: Session) {
  banner('')
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
  hideIncoming()
  const token = session?.token
  accountCache = null
  void teardown()
  signal?.stop()
  signal = null
  session = null
  const saved = loadSession()
  if (!saved || saved.token === token) saveSession(null)
  show('login-view')
}

function showTab(admin: boolean) {
  $('call-panel').classList.toggle('hidden', admin)
  $('admin-panel').classList.toggle('hidden', !admin)
  $('tab-call').classList.toggle('on', !admin)
  $('tab-admin').classList.toggle('on', admin)
  if (admin) void refreshUsers()
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
$('mini-hangup').onclick = hangup
$('btn-minimize').onclick = () => {
  if (!current || !isAdmin()) return
  current.minimized = true
  renderCall()
}
$('mini-expand').onclick = () => {
  if (!current) return
  current.minimized = false
  renderCall()
}
$('mini-mute').onclick = () => $('btn-mute').click()
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
$('user-add-open').onclick = () => {
  $('user-error').textContent = ''
  ;($('user-form') as HTMLFormElement).reset()
  $('user-add-dialog').classList.remove('hidden')
}
$('user-add-cancel').onclick = () => $('user-add-dialog').classList.add('hidden')
$('user-form').addEventListener('submit', async (ev) => {
  ev.preventDefault()
  if (!session) return
  const form = ev.target as HTMLFormElement
  const data = new FormData(form)
  $('user-error').textContent = ''
  try {
    await createUser(session.token, String(data.get('username')), String(data.get('password')))
    form.reset()
    $('user-add-dialog').classList.add('hidden')
    await refreshUsers()
  } catch (e) {
    if (e instanceof UnauthorizedError) logout()
    else $('user-error').textContent = explain(e)
  }
})
$('user-action-close').onclick = closeUserAction
$('user-action-delete').onclick = () => {
  if (!actionUser || $<HTMLButtonElement>('user-action-delete').disabled) return
  closeUserAction()
  $('user-confirm-error').textContent = ''
  $('user-confirm-text').textContent = `确认删除用户 ${actionUser}？该用户会立即退出登录。`
  $('user-confirm-dialog').classList.remove('hidden')
}
$('user-confirm-cancel').onclick = () => $('user-confirm-dialog').classList.add('hidden')
$('user-confirm-ok').onclick = () => void removeUser(actionUser)

function bindVolume(inputId: string, textId: string, key: 'speaker' | 'mic') {
  const input = $<HTMLInputElement>(inputId)
  const preview = () => {
    const v = Number(input.value)
    showVolume(textId, v)
    queueVolume({ [key]: v })
  }
  input.addEventListener('input', preview)
  input.addEventListener('change', () => {
    preview()
    clearTimeout(volumeTimer)
    void flushVolume()
  })
}
bindVolume('volume-speaker', 'volume-speaker-text', 'speaker')
bindVolume('volume-mic', 'volume-mic-text', 'mic')
$('volume-close').onclick = async () => {
  clearTimeout(volumeTimer)
  await flushVolume()
  closeVolume()
}

$('incoming-accept').onclick = () => {
  const room = incoming?.room
  hideIncoming()
  if (room) joinRoom(room)
}
$('incoming-reject').onclick = () => {
  const room = incoming?.room
  hideIncoming()
  if (room) signal?.send({ type: 'reject', room })
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
