<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { api } from './api'

const { t, locale } = useI18n()

const user = ref(null)
const snapshot = ref({ devices: [], calls: [], callTotal: 0, groups: {}, runtime: {} })
const users = ref([])
const activePanel = ref('overview')
const authMode = ref('login')
const authOpen = ref(false)
const deviceEditorOpen = ref(false)
const editingDeviceId = ref(null)
const authForm = ref({ username: '', callsign: '', email: '', password: '' })
const authConfirmPassword = ref('')
const authShowPassword = ref(false)
const authShowConfirmPassword = ref(false)
const userForm = ref({ username: '', callsign: '', email: '', password: '', role: 'ham', enabled: true })
const deviceDrafts = ref({})
const busy = ref(false)
const message = ref('')
const nowTick = ref(Date.now())
const wsConnected = ref(false)

let snapshotIntervalId = null
let durationIntervalId = null
let ws = null
let wsReconnectTimer = null
const authSessionHintKey = 'auth_session_hint'

const isAdmin = computed(() => user.value?.role === 'admin')
const onlineCount = computed(() => snapshot.value.devices.filter((device) => device.online).length)
const sortedDevices = computed(() =>
  [...snapshot.value.devices].sort((a, b) => Number(b.id || 0) - Number(a.id || 0))
)
const myDevices = computed(() => {
  if (!user.value) return []
  const callsign = normalizeCallsign(user.value.callsign)
  if (!callsign) return []
  return sortedDevices.value.filter((device) => normalizeCallsign(device.callsign) === callsign)
})
const editingDevice = computed(() =>
  sortedDevices.value.find((device) => device.id === editingDeviceId.value) || null
)
const sortedCalls = computed(() =>
  [...snapshot.value.calls].sort((a, b) => {
    const timeDiff = new Date(b.createdAt || 0).getTime() - new Date(a.createdAt || 0).getTime()
    if (timeDiff !== 0) return timeDiff
    return Number(b.id || 0) - Number(a.id || 0)
  })
)
const activeCalls = computed(() => sortedCalls.value.filter((call) => isActiveCall(call)))
const recentHistoryCalls = computed(() => sortedCalls.value.filter((call) => !isActiveCall(call)))
const liveHeadlineCall = computed(() => activeCalls.value[0] || null)
const overviewCalls = computed(() => sortedCalls.value)
const navItems = computed(() => {
  const items = [
    { key: 'overview', label: t('app.overview') },
    { key: 'devices', label: t('app.devices') }
  ]
  if (user.value) items.splice(1, 0, { key: 'my-devices', label: t('app.myDevices') })
  if (user.value && isAdmin.value) items.push({ key: 'accounts', label: t('app.accounts') })
  return items
})

function setLocale(nextLocale) {
  locale.value = nextLocale
  localStorage.setItem('locale', nextLocale)
}

function normalizeCallsign(value) {
  return String(value || '').toUpperCase().replace(/[^A-Z0-9]/g, '')
}

function isValidCallsign(value) {
  const callsign = normalizeCallsign(value)
  if (callsign.length < 4 || callsign.length > 6) return false
  const digits = callsign.replace(/[^0-9]/g, '').length
  const letters = callsign.replace(/[^A-Z]/g, '').length
  return digits === 1 && letters + digits === callsign.length
}

function syncAuthCallsign() {
  authForm.value.callsign = normalizeCallsign(authForm.value.callsign)
}

function syncUserCallsign() {
  userForm.value.callsign = normalizeCallsign(userForm.value.callsign)
}

function fmtTime(value) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return '-'
  return date.toLocaleString(locale.value === 'zh-CN' ? 'zh-CN' : 'en-US')
}

function isZeroTime(value) {
  return !value || value === '0001-01-01T00:00:00Z' || String(value).startsWith('0001-01-01T00:00:00')
}

function isActiveCall(call) {
  return isZeroTime(call?.endedAt)
}

function callStartedAt(call) {
  const startedAt = new Date(call?.createdAt || 0).getTime()
  return Number.isFinite(startedAt) ? startedAt : 0
}

function callEndedAt(call) {
  if (!call || isZeroTime(call.endedAt)) return 0
  const endedAt = new Date(call.endedAt).getTime()
  return Number.isFinite(endedAt) ? endedAt : 0
}

function callDurationSeconds(call) {
  const startedAt = callStartedAt(call)
  if (!startedAt) return 0
  if (isActiveCall(call)) return Math.max(0, Math.floor((nowTick.value - startedAt) / 1000))
  const endedAt = callEndedAt(call)
  if (endedAt) return Math.max(0, Math.floor((endedAt - startedAt) / 1000))
  if (Number(call?.durationMs) > 0) return Math.max(0, Math.floor(Number(call.durationMs) / 1000))
  return 0
}

function fmtDuration(call) {
  const total = callDurationSeconds(call)
  const hours = Math.floor(total / 3600)
  const minutes = Math.floor((total % 3600) / 60)
  const seconds = total % 60
  if (hours > 0) return `${hours}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`
  return `${minutes}:${String(seconds).padStart(2, '0')}`
}

function callTitle(call) {
  return call?.sourceCallsign || (call?.sourceDmrid ? String(call.sourceDmrid) : '-') 
}

function callSourceDevice(call) {
  return call?.sourceName || '-'
}

function callTypeLabel(call) {
  if (call?.callType === 'private') return t('call.private')
  if (call?.callType === 'analog') return t('call.analog')
  return t('call.group')
}

function callStatusLabel(call) {
  return isActiveCall(call) ? t('call.live') : callTypeLabel(call)
}

function roleLabel(role) {
  return role === 'admin' ? t('auth.roleAdminLabel') : t('auth.roleHamLabel')
}

function slotSubscriptions(sourceKey, slot) {
  return snapshot.value.groups?.[sourceKey]?.slots?.[String(slot)] || []
}

function groupSummary(sourceKey, slot, kind = 'static') {
  const values = slotSubscriptions(sourceKey, slot)
    .filter((item) => item.kind === kind)
    .map((item) => item.groupId)
  return values.length ? values.join(', ') : '-'
}

function canEditDevice(device) {
  if (!user.value) return false
  if (isAdmin.value) return true
  return normalizeCallsign(device.callsign) === normalizeCallsign(user.value.callsign)
}

function isHyteraDevice(device) {
  const protocol = String(device?.protocol || '').toLowerCase()
  const sourceKey = String(device?.sourceKey || '').toLowerCase()
  return protocol === 'hytera' || sourceKey.startsWith('hytera:')
}

function makeDeviceDraft(device) {
  return {
    name: device.name || '',
    callsign: device.callsign || '',
    dmrid: device.dmrid || '',
    model: device.model || '',
    description: device.description || '',
    location: device.location || '',
    notes: device.notes || '',
    devicePassword: device.devicePassword || '',
    nrlServerAddr: device.nrlServerAddr || '',
    nrlServerPort: device.nrlServerPort || '',
    nrlSsid: device.nrlSsid || '',
    staticSlot1: slotSubscriptions(device.sourceKey, 1).filter((item) => item.kind === 'static').map((item) => item.groupId).join(','),
    staticSlot2: slotSubscriptions(device.sourceKey, 2).filter((item) => item.kind === 'static').map((item) => item.groupId).join(',')
  }
}

function rebuildDeviceDrafts() {
  const activeEditId = deviceEditorOpen.value ? editingDeviceId.value : null
  const currentDrafts = deviceDrafts.value
  deviceDrafts.value = Object.fromEntries(snapshot.value.devices.map((device) => {
    if (activeEditId === device.id && currentDrafts[device.id]) {
      return [device.id, currentDrafts[device.id]]
    }
    return [device.id, makeDeviceDraft(device)]
  }))
}

async function loadSession() {
  if (localStorage.getItem(authSessionHintKey) !== '1') {
    user.value = null
    users.value = []
    connectWS()
    return
  }
  try {
    user.value = await api.me()
    if (isAdmin.value) users.value = await api.listUsers()
    localStorage.setItem(authSessionHintKey, '1')
    connectWS()
  } catch {
    user.value = null
    users.value = []
    localStorage.removeItem(authSessionHintKey)
    connectWS()
    if (activePanel.value === 'accounts' || activePanel.value === 'my-devices') activePanel.value = 'overview'
  }
}

async function loadSnapshot() {
  snapshot.value = await api.snapshot()
  rebuildDeviceDrafts()
}

function applySnapshot(nextSnapshot) {
  snapshot.value = {
    devices: nextSnapshot?.devices || [],
    calls: nextSnapshot?.calls || [],
    callTotal: Number(nextSnapshot?.callTotal || 0),
    groups: nextSnapshot?.groups || {},
    runtime: nextSnapshot?.runtime || {}
  }
  rebuildDeviceDrafts()
}

function upsertDevice(device) {
  const devices = [...snapshot.value.devices]
  const index = devices.findIndex((item) => item.id === device.id)
  if (index >= 0) devices[index] = device
  else devices.push(device)
  snapshot.value = { ...snapshot.value, devices }
  rebuildDeviceDrafts()
}

function removeDevice(device) {
  const devices = snapshot.value.devices.filter((item) => item.id !== device.id)
  const groups = { ...snapshot.value.groups }
  if (device?.sourceKey) delete groups[device.sourceKey]
  snapshot.value = { ...snapshot.value, devices, groups }
  if (editingDeviceId.value === device.id) {
    editingDeviceId.value = null
    deviceEditorOpen.value = false
  }
  rebuildDeviceDrafts()
}

function appendCall(call) {
  const calls = [...snapshot.value.calls]
  const index = call.id ? calls.findIndex((item) => item.id === call.id) : -1
  const isNew = index < 0
  if (index >= 0) calls[index] = call
  else calls.push(call)
  calls.sort((a, b) => new Date(b.createdAt || 0).getTime() - new Date(a.createdAt || 0).getTime())
  snapshot.value = {
    ...snapshot.value,
    calls: calls.slice(0, 50),
    callTotal: isNew ? Number(snapshot.value.callTotal || 0) + 1 : Number(snapshot.value.callTotal || 0)
  }
}

function wsURL() {
  if (snapshot.value.runtime?.websocketUrl) return `${snapshot.value.runtime.websocketUrl.replace(/\/$/, '')}/ws`
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  return `${proto}://${window.location.host}/ws`
}

function scheduleReconnect() {
  if (wsReconnectTimer) return
  wsReconnectTimer = window.setTimeout(() => {
    wsReconnectTimer = null
    connectWS()
  }, 1200)
}

function closeWS() {
  wsConnected.value = false
  if (wsReconnectTimer) {
    window.clearTimeout(wsReconnectTimer)
    wsReconnectTimer = null
  }
  if (ws) {
    const current = ws
    ws = null
    current.onclose = null
    current.close()
  }
}

function connectWS() {
  if (ws) return
  try {
    ws = new WebSocket(wsURL())
  } catch {
    ws = null
    scheduleReconnect()
    return
  }
  ws.onopen = () => {
    wsConnected.value = true
  }
  ws.onmessage = (event) => {
    const payload = JSON.parse(event.data)
    if (payload.type === 'snapshot' && payload.snapshot) applySnapshot(payload.snapshot)
    if (payload.type === 'device_updated' && payload.device) upsertDevice(payload.device)
    if (payload.type === 'device_deleted' && payload.device) removeDevice(payload.device)
    if (payload.type === 'call_recorded' && payload.call) appendCall(payload.call)
  }
  ws.onclose = () => {
    wsConnected.value = false
    ws = null
    scheduleReconnect()
  }
  ws.onerror = () => {
    wsConnected.value = false
  }
}

async function submitAuth() {
  busy.value = true
  message.value = ''
  try {
    if (authMode.value === 'login') {
      user.value = await api.login({ username: authForm.value.username, password: authForm.value.password })
      localStorage.setItem(authSessionHintKey, '1')
      authOpen.value = false
      authForm.value = { username: '', callsign: '', email: '', password: '' }
      authConfirmPassword.value = ''
      authShowPassword.value = false
      authShowConfirmPassword.value = false
      await loadSession()
      await loadSnapshot()
    } else {
      syncAuthCallsign()
      if (!isValidCallsign(authForm.value.callsign)) throw new Error(t('auth.callsignRule'))
      if (authForm.value.password.length < 8) throw new Error(t('auth.passwordRule'))
      if (authForm.value.password !== authConfirmPassword.value) throw new Error(t('auth.passwordMismatch'))
      await api.register(authForm.value)
      message.value = t('auth.registerSuccess')
      authMode.value = 'login'
      authForm.value.password = ''
      authConfirmPassword.value = ''
      authShowPassword.value = false
      authShowConfirmPassword.value = false
    }
  } catch (error) {
    message.value = error.message || t('auth.loginError')
  } finally {
    busy.value = false
  }
}

async function logout() {
  await api.logout()
  user.value = null
  users.value = []
  localStorage.removeItem(authSessionHintKey)
  closeWS()
  connectWS()
  if (activePanel.value === 'accounts' || activePanel.value === 'my-devices') activePanel.value = 'overview'
}

async function saveDevice(device) {
  const draft = deviceDrafts.value[device.id]
  const hyteraDevice = isHyteraDevice(device)
  const payload = {
    name: draft.name,
    notes: draft.notes,
    model: draft.model,
    description: draft.description,
    location: draft.location,
    devicePassword: draft.devicePassword,
    staticSlot1: parseGroups(draft.staticSlot1),
    staticSlot2: parseGroups(draft.staticSlot2)
  }
  if (hyteraDevice) {
    payload.nrlServerAddr = draft.nrlServerAddr
    payload.nrlServerPort = Number(draft.nrlServerPort || 0)
    payload.nrlSsid = Number(draft.nrlSsid || 0)
  }
  if (isAdmin.value) {
    payload.callsign = draft.callsign
    payload.dmrid = Number(draft.dmrid || 0)
  }
  await api.updateDevice(device.id, payload)
  message.value = t('device.saveSuccess')
  deviceEditorOpen.value = false
  editingDeviceId.value = null
  await loadSnapshot()
}

async function deleteDevice(device) {
  if (!isAdmin.value) return
  if (!window.confirm(`${t('app.delete')} ${device.name || device.callsign || device.sourceKey}?`)) return
  await api.deleteDevice(device.id)
  if (editingDeviceId.value === device.id) {
    editingDeviceId.value = null
    deviceEditorOpen.value = false
  }
}

async function createUser() {
  syncUserCallsign()
  if (!isValidCallsign(userForm.value.callsign)) throw new Error(t('auth.callsignRule'))
  await api.createUser(userForm.value)
  userForm.value = { username: '', callsign: '', email: '', password: '', role: 'ham', enabled: true }
  users.value = await api.listUsers()
}

async function toggleUserEnabled(account) {
  await api.updateUser(account.id, { enabled: !account.enabled })
  users.value = await api.listUsers()
}

async function changeRole(account) {
  await api.updateUser(account.id, { role: account.role === 'admin' ? 'ham' : 'admin' })
  users.value = await api.listUsers()
}

async function resetPassword(account) {
  const password = window.prompt(t('auth.newPasswordPrompt'))
  if (!password) return
  await api.resetUserPassword(account.id, password)
}

async function removeUser(account) {
  if (!window.confirm(`${t('app.delete')} ${account.username}?`)) return
  await api.deleteUser(account.id)
  users.value = await api.listUsers()
}

function openAuth(mode) {
  authMode.value = mode
  authOpen.value = true
  message.value = ''
  authForm.value.password = ''
  authConfirmPassword.value = ''
  authShowPassword.value = false
  authShowConfirmPassword.value = false
}

function openDeviceEditor(device) {
  editingDeviceId.value = device.id
  deviceEditorOpen.value = true
}

function parseGroups(value) {
  return String(value || '')
    .split(/[\s,]+/)
    .map((item) => Number(item.trim()))
    .filter((item, index, list) => Number.isFinite(item) && item > 0 && list.indexOf(item) === index)
}

onMounted(async () => {
  await Promise.all([loadSession(), loadSnapshot()])
  connectWS()
  durationIntervalId = window.setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
  snapshotIntervalId = window.setInterval(() => {
    loadSnapshot().catch(() => {})
  }, 10000)
})

onUnmounted(() => {
  if (durationIntervalId) window.clearInterval(durationIntervalId)
  if (snapshotIntervalId) window.clearInterval(snapshotIntervalId)
  closeWS()
})
</script>

<template>
  <div class="app-shell">
    <div class="aurora aurora-a"></div>
    <div class="aurora aurora-b"></div>

    <section class="topbar glass">
      <div>
        <p class="eyebrow">NRL DMR LINK</p>
        <h1>{{ t('app.title') }}</h1>
        <p class="topbar-subtitle">{{ t('app.subtitle') }}</p>
      </div>
      <div class="topbar-actions">
        <div class="locale-switch">
          <button :class="{ active: locale === 'zh-CN' }" @click="setLocale('zh-CN')">中文</button>
          <button :class="{ active: locale === 'en' }" @click="setLocale('en')">EN</button>
        </div>
        <template v-if="user">
          <button class="ghost">
            {{ user.username }} / {{ user.callsign || '-' }}
            <span class="muted-inline">· {{ roleLabel(user.role) }}</span>
            <span class="muted-inline">· {{ wsConnected ? t('call.wsOnline') : t('call.wsOffline') }}</span>
          </button>
          <button class="primary" @click="logout">{{ t('app.logout') }}</button>
        </template>
        <template v-else>
          <button class="ghost" @click="openAuth('login')">{{ t('app.login') }}</button>
          <button class="primary" @click="openAuth('register')">{{ t('app.register') }}</button>
        </template>
      </div>
    </section>

    <section class="metrics-row">
      <article class="metric-card glass">
        <span>{{ t('dashboard.totalDevices') }}</span>
        <strong>{{ snapshot.devices.length }}</strong>
      </article>
      <article class="metric-card glass">
        <span>{{ t('dashboard.onlineDevices') }}</span>
        <strong>{{ onlineCount }}</strong>
      </article>
      <article class="metric-card glass">
        <span>{{ t('dashboard.calls') }}</span>
        <strong>{{ snapshot.callTotal || 0 }}</strong>
      </article>
      <article class="metric-card glass">
        <span>{{ t('dashboard.activeCalls') }}</span>
        <strong>{{ activeCalls.length }}</strong>
      </article>
    </section>

    <section class="workspace">
      <aside class="sidebar glass">
        <button v-for="item in navItems" :key="item.key" :class="{ active: activePanel === item.key }" @click="activePanel = item.key">
          {{ item.label }}
        </button>
      </aside>

      <main
        :class="[
          'content',
          'glass',
          {
            'content-scroll': ['devices', 'accounts', 'my-devices'].includes(activePanel),
            'content-overview': activePanel === 'overview',
            'content-calls': activePanel === 'calls'
          }
        ]"
      >
        <template v-if="activePanel === 'overview'">
          <section class="overview-panel">
            <div class="section-head">
              <h2>{{ t('app.overview') }}</h2>
            </div>
            <div class="overview-grid">
              <article class="glass inset runtime-card call-focus-panel">
                <div class="call-panel-head">
                  <div>
                    <h3>{{ t('app.calls') }}</h3>
                    <p class="hint">{{ t('call.priorityHint') }}</p>
                  </div>
                  <span class="call-count-badge">{{ activeCalls.length }} {{ t('call.live') }}</span>
                </div>
                <article v-if="liveHeadlineCall" class="call-hero live">
                  <div class="call-hero-top">
                    <div class="call-primary">
                      <span class="pulse-dot"></span>
                      <strong>{{ callTitle(liveHeadlineCall) }}</strong>
                      <span class="voice-wave" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></span>
                    </div>
                    <span class="call-status-badge live">{{ t('call.live') }}</span>
                  </div>
                  <div class="call-hero-body">
                    <strong class="call-hero-duration">{{ fmtDuration(liveHeadlineCall) }}</strong>
                    <div class="call-hero-meta">
                      <span>TG {{ liveHeadlineCall.dstId || '-' }}</span>
                      <span>TS{{ liveHeadlineCall.slot || '-' }}</span>
                      <span>{{ t('call.callsign') }} {{ liveHeadlineCall.sourceCallsign || '-' }}</span>
                      <span>{{ callSourceDevice(liveHeadlineCall) }}</span>
                      <span>{{ t('call.dmrid') }} {{ liveHeadlineCall.sourceDmrid || '-' }}</span>
                      <span>{{ fmtTime(liveHeadlineCall.createdAt) }}</span>
                    </div>
                  </div>
                </article>
                <div class="call-mini-list">
                  <article
                    v-for="call in overviewCalls"
                    :key="call.id"
                    :class="['call-card', { live: isActiveCall(call) }]"
                  >
                    <div class="call-card-inline">
                      <div class="call-primary">
                        <span v-if="isActiveCall(call)" class="pulse-dot"></span>
                        <strong>{{ callTitle(call) }}</strong>
                      </div>
                      <div class="call-card-meta call-card-meta-inline">
                        <span :class="['call-status-badge', { live: isActiveCall(call) }]">{{ callStatusLabel(call) }}</span>
                        <span>{{ call.dstId || '-' }}</span>
                        <span>TS{{ call.slot || '-' }}</span>
                        <span>{{ call.sourceCallsign || '-' }}</span>
                        <span>{{ callSourceDevice(call) }}</span>
                        <span>{{ call.sourceDmrid || '-' }}</span>
                        <span>{{ fmtDuration(call) }}</span>
                        <span>{{ fmtTime(call.createdAt) }}</span>
                      </div>
                    </div>
                  </article>
                  <div v-if="!overviewCalls.length" class="hint">{{ t('call.empty') }}</div>
                </div>
              </article>

              <article class="glass inset runtime-card runtime-panel">
                <h3>{{ t('dashboard.systemInfo') }}</h3>
                <div class="kv-list compact">
                  <div class="kv-row"><span>URL</span><strong>{{ snapshot.runtime.accessUrl || '-' }}</strong></div>
                  <div class="kv-row"><span>IPSC</span><strong>{{ snapshot.runtime.ipscListen || '-' }}</strong></div>
                  <div class="kv-row"><span>Hytera P2P</span><strong>{{ snapshot.runtime.hyteraP2pListen || '-' }}</strong></div>
                  <div class="kv-row"><span>Hytera DMR</span><strong>{{ snapshot.runtime.hyteraDmrListen || '-' }}</strong></div>
                  <div class="kv-row"><span>Hytera RDAC</span><strong>{{ snapshot.runtime.hyteraRdacListen || '-' }}</strong></div>
                </div>
              </article>
            </div>
          </section>
        </template>

        <template v-else-if="activePanel === 'calls'">
          <section class="calls-panel">
            <div class="section-head">
              <h2>{{ t('app.calls') }}</h2>
              <span class="muted-inline">{{ activeCalls.length }} {{ t('call.live') }}</span>
            </div>
            <section v-if="activeCalls.length" class="live-call-band">
              <article v-for="call in activeCalls" :key="call.id" class="call-hero live compact">
                <div class="call-hero-top">
                  <div class="call-primary">
                    <span class="pulse-dot"></span>
                    <strong>{{ callTitle(call) }}</strong>
                    <span class="voice-wave" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></span>
                  </div>
                  <strong class="call-hero-duration">{{ fmtDuration(call) }}</strong>
                </div>
                <div class="call-hero-meta">
                  <span>{{ callTypeLabel(call) }}</span>
                  <span>TG {{ call.dstId || '-' }}</span>
                  <span>TS{{ call.slot || '-' }}</span>
                  <span>{{ t('call.callsign') }} {{ call.sourceCallsign || '-' }}</span>
                  <span>{{ callSourceDevice(call) }}</span>
                  <span>{{ t('call.dmrid') }} {{ call.sourceDmrid || '-' }}</span>
                  <span>{{ fmtTime(call.createdAt) }}</span>
                </div>
              </article>
            </section>
            <div class="call-table">
              <article
                v-for="call in recentHistoryCalls"
                :key="call.id"
                :class="['call-card', 'call-card-detailed', { live: isActiveCall(call) }]"
              >
                <div class="call-card-top">
                  <div class="call-primary">
                    <span v-if="isActiveCall(call)" class="pulse-dot"></span>
                    <strong>{{ callTitle(call) }}</strong>
                  </div>
                  <div class="call-card-side">
                    <span :class="['call-status-badge', { live: isActiveCall(call) }]">{{ callStatusLabel(call) }}</span>
                    <strong class="call-duration">{{ fmtDuration(call) }}</strong>
                  </div>
                </div>
                <div class="call-card-grid">
                  <div class="call-field"><span>{{ t('call.target') }}</span><strong>TG {{ call.dstId || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.slot') }}</span><strong>TS{{ call.slot || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.startedAt') }}</span><strong>{{ fmtTime(call.createdAt) }}</strong></div>
                  <div class="call-field"><span>{{ t('call.type') }}</span><strong>{{ callTypeLabel(call) }}</strong></div>
                  <div class="call-field"><span>{{ t('call.dmrid') }}</span><strong>{{ call.sourceDmrid || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.sourceId') }}</span><strong>{{ call.srcId || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.callsign') }}</span><strong>{{ call.sourceCallsign || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.sourceDevice') }}</span><strong>{{ call.sourceName || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.frontend') }}</span><strong>{{ call.frontend || '-' }}</strong></div>
                  <div class="call-field"><span>{{ t('call.stream') }}</span><strong>{{ call.streamId || '-' }}</strong></div>
                  <div class="call-field call-field-wide"><span>{{ t('call.address') }}</span><strong>{{ call.fromIp || '-' }}{{ call.fromPort ? `:${call.fromPort}` : '' }}</strong></div>
                </div>
              </article>
              <div v-if="!sortedCalls.length" class="hint">{{ t('call.empty') }}</div>
            </div>
          </section>
        </template>

        <template v-else-if="activePanel === 'devices'">
          <div class="section-head">
            <h2>{{ t('app.devices') }}</h2>
          </div>
          <div class="device-table-wrap">
            <table class="device-table">
              <thead>
                <tr>
                  <th>{{ t('device.name') }}</th>
                  <th>{{ t('device.status') }}</th>
                  <th>{{ t('device.callsign') }}</th>
                  <th>{{ t('device.dmrid') }}</th>
                  <th v-if="isAdmin">{{ t('device.address') }}</th>
                  <th>{{ t('device.model') }}</th>
                  <th>{{ t('device.location') }}</th>
                  <th>TS1 {{ t('device.staticGroups') }}</th>
                  <th>TS1 {{ t('device.dynamicGroups') }}</th>
                  <th>TS2 {{ t('device.staticGroups') }}</th>
                  <th>TS2 {{ t('device.dynamicGroups') }}</th>
                  <th>{{ t('device.protocol') }}</th>
                  <th>{{ t('device.description') }}</th>
                  <th>{{ t('device.notes') }}</th>
                  <th>{{ t('app.actions') }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="device in sortedDevices" :key="device.id">
                  <td>{{ device.name || device.sourceKey }}</td>
                  <td><span :class="['table-status', { online: device.online }]">{{ device.online ? t('call.online') : t('call.offline') }}</span></td>
                  <td>{{ device.callsign || '-' }}</td>
                  <td>{{ device.dmrid || '-' }}</td>
                  <td v-if="isAdmin">{{ device.ip ? `${device.ip}${device.port ? `:${device.port}` : ''}` : '-' }}</td>
                  <td>{{ device.model || '-' }}</td>
                  <td>{{ device.location || '-' }}</td>
                  <td>{{ groupSummary(device.sourceKey, 1, 'static') }}</td>
                  <td>{{ groupSummary(device.sourceKey, 1, 'dynamic') }}</td>
                  <td>{{ groupSummary(device.sourceKey, 2, 'static') }}</td>
                  <td>{{ groupSummary(device.sourceKey, 2, 'dynamic') }}</td>
                  <td>{{ device.protocol || '-' }}</td>
                  <td>{{ device.description || '-' }}</td>
                  <td>{{ device.notes || '-' }}</td>
                  <td>
                    <template v-if="canEditDevice(device) || isAdmin">
                      <button v-if="canEditDevice(device)" class="ghost" @click="openDeviceEditor(device)">{{ t('app.edit') }}</button>
                      <button v-if="isAdmin" class="ghost danger" @click="deleteDevice(device)">{{ t('app.delete') }}</button>
                    </template>
                    <span v-else class="muted-inline">-</span>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </template>

        <template v-else-if="activePanel === 'my-devices'">
          <div class="section-head">
            <h2>{{ t('app.myDevices') }}</h2>
            <span class="muted-inline">{{ t('device.myDevicesHint') }}</span>
          </div>
          <div class="my-device-list">
            <article v-for="device in myDevices" :key="device.id" class="glass inset my-device-card">
              <div class="my-device-head">
                <div>
                  <strong>{{ device.name || device.sourceKey }}</strong>
                  <p class="muted-inline">{{ device.callsign || '-' }} · {{ device.protocol || '-' }}</p>
                </div>
                <span :class="['table-status', { online: device.online }]">{{ device.online ? t('call.online') : t('call.offline') }}</span>
              </div>
              <div class="kv-list compact">
                <div class="kv-row"><span>{{ t('device.callsign') }}</span><strong>{{ device.callsign || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.dmrid') }}</span><strong>{{ device.dmrid || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.model') }}</span><strong>{{ device.model || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.location') }}</span><strong>{{ device.location || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.password') }}</span><strong>{{ device.devicePassword || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device)" class="kv-row"><span>{{ t('device.nrlServerAddr') }}</span><strong>{{ device.nrlServerAddr || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device)" class="kv-row"><span>{{ t('device.nrlServerPort') }}</span><strong>{{ device.nrlServerPort || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device)" class="kv-row"><span>{{ t('device.nrlSsid') }}</span><strong>{{ device.nrlSsid || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.notes') }}</span><strong>{{ device.notes || '-' }}</strong></div>
              </div>
              <div class="account-actions">
                <button v-if="canEditDevice(device)" class="primary" @click="openDeviceEditor(device)">{{ t('app.edit') }}</button>
                <button v-if="isAdmin" class="ghost danger" @click="deleteDevice(device)">{{ t('app.delete') }}</button>
              </div>
            </article>
            <div v-if="!myDevices.length" class="hint">{{ t('device.noOwnedDevices') }}</div>
          </div>
        </template>

        <template v-else>
          <div class="section-head">
            <h2>{{ t('app.accounts') }}</h2>
          </div>
          <div class="account-layout">
            <form class="glass inset form-grid" @submit.prevent="createUser">
              <input v-model="userForm.username" :placeholder="t('auth.username')" />
              <input v-model="userForm.callsign" :placeholder="t('auth.callsign')" @input="syncUserCallsign" />
              <input v-model="userForm.email" :placeholder="t('auth.email')" />
              <input v-model="userForm.password" type="password" :placeholder="t('auth.password')" />
              <select v-model="userForm.role">
                <option value="ham">{{ t('auth.ham') }}</option>
                <option value="admin">{{ t('auth.admin') }}</option>
              </select>
              <label class="checkbox-line"><input v-model="userForm.enabled" type="checkbox" /> {{ t('app.enabled') }}</label>
              <button class="primary">{{ t('app.create') }}</button>
            </form>
            <div class="account-list">
              <article v-for="account in users" :key="account.id" class="account-card">
                <div>
                  <strong>{{ account.username }}</strong>
                  <p>{{ account.callsign }} · {{ account.email }}</p>
                </div>
                <div class="account-actions">
                  <button class="ghost" @click="toggleUserEnabled(account)">{{ account.enabled ? t('app.disable') : t('app.enable') }}</button>
                  <button class="ghost" @click="changeRole(account)">{{ account.role === 'admin' ? t('auth.ham') : t('auth.admin') }}</button>
                  <button class="ghost" @click="resetPassword(account)">{{ t('app.resetPassword') }}</button>
                  <button class="ghost danger" @click="removeUser(account)">{{ t('app.delete') }}</button>
                </div>
              </article>
            </div>
          </div>
        </template>
      </main>
    </section>

    <div v-if="authOpen" class="modal-backdrop">
      <section class="auth-modal glass">
        <div class="section-head">
          <h2>{{ authMode === 'login' ? t('app.login') : t('app.register') }}</h2>
          <button class="ghost" @click="authOpen = false">{{ t('app.close') }}</button>
        </div>
        <div class="tab-row">
          <button :class="{ active: authMode === 'login' }" @click="openAuth('login')">{{ t('app.login') }}</button>
          <button :class="{ active: authMode === 'register' }" @click="openAuth('register')">{{ t('app.register') }}</button>
        </div>
        <form class="form-grid" @submit.prevent="submitAuth">
          <input v-model="authForm.username" :placeholder="t('auth.username')" />
          <input v-if="authMode === 'register'" v-model="authForm.callsign" :placeholder="t('auth.callsign')" @input="syncAuthCallsign" />
          <input v-if="authMode === 'register'" v-model="authForm.email" :placeholder="t('auth.email')" />
          <div class="password-field">
            <input
              v-model="authForm.password"
              :type="authShowPassword ? 'text' : 'password'"
              :placeholder="t('auth.password')"
            />
            <button type="button" class="password-toggle" @click="authShowPassword = !authShowPassword">
              {{ authShowPassword ? t('auth.hidePassword') : t('auth.showPassword') }}
            </button>
          </div>
          <div v-if="authMode === 'register'" class="password-field">
            <input
              v-model="authConfirmPassword"
              :type="authShowConfirmPassword ? 'text' : 'password'"
              :placeholder="t('auth.confirmPassword')"
            />
            <button type="button" class="password-toggle" @click="authShowConfirmPassword = !authShowConfirmPassword">
              {{ authShowConfirmPassword ? t('auth.hidePassword') : t('auth.showPassword') }}
            </button>
          </div>
          <button class="primary" :disabled="busy">{{ authMode === 'login' ? t('app.login') : t('app.register') }}</button>
        </form>
        <p class="hint">{{ t('auth.registerHint') }}</p>
        <p v-if="message" class="message">{{ message }}</p>
      </section>
    </div>

    <div v-if="deviceEditorOpen && editingDevice" class="modal-backdrop">
      <section class="auth-modal glass device-editor-modal">
        <div class="section-head">
          <h2>{{ t('app.edit') }} · {{ editingDevice.name || editingDevice.callsign || editingDevice.sourceKey }}</h2>
          <button class="ghost" @click="deviceEditorOpen = false">{{ t('app.close') }}</button>
        </div>
        <form class="form-grid device-editor-form" @submit.prevent="saveDevice(editingDevice)">
          <div class="device-editor-grid">
          <label class="field-block">
            <span>{{ t('device.name') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].name" :placeholder="t('device.name')" />
          </label>
          <label class="field-block">
            <span>{{ t('device.model') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].model" :placeholder="t('device.model')" />
          </label>
          <label v-if="isAdmin" class="field-block">
            <span>{{ t('device.callsign') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].callsign" :placeholder="t('device.callsign')" />
          </label>
          <label v-if="isAdmin" class="field-block">
            <span>{{ t('device.dmrid') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].dmrid" :placeholder="t('device.dmrid')" />
          </label>
          <label class="field-block">
            <span>{{ t('device.location') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].location" :placeholder="t('device.location')" />
          </label>
          <label class="field-block">
            <span>{{ t('device.password') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].devicePassword" :placeholder="t('device.password')" />
          </label>
          <label v-if="isHyteraDevice(editingDevice)" class="field-block">
            <span>{{ t('device.nrlServerAddr') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].nrlServerAddr" :placeholder="t('device.nrlServerAddr')" />
          </label>
          <label v-if="isHyteraDevice(editingDevice)" class="field-block">
            <span>{{ t('device.nrlServerPort') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].nrlServerPort" :placeholder="t('device.nrlServerPort')" />
          </label>
          <label v-if="isHyteraDevice(editingDevice)" class="field-block">
            <span>{{ t('device.nrlSsid') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].nrlSsid" :placeholder="t('device.nrlSsid')" />
          </label>
          <label class="field-block field-span-2">
            <span>{{ t('device.description') }}</span>
            <textarea v-model="deviceDrafts[editingDevice.id].description" rows="3" :placeholder="t('device.description')"></textarea>
          </label>
          <label class="field-block field-span-2">
            <span>{{ t('device.notes') }}</span>
            <textarea v-model="deviceDrafts[editingDevice.id].notes" rows="3" :placeholder="t('device.notes')"></textarea>
          </label>
          <label class="field-block">
            <span>{{ t('device.ts1') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].staticSlot1" :placeholder="t('device.ts1')" />
          </label>
          <label class="field-block">
            <span>{{ t('device.ts2') }}</span>
            <input v-model="deviceDrafts[editingDevice.id].staticSlot2" :placeholder="t('device.ts2')" />
          </label>
          </div>
          <div class="device-editor-actions">
            <button type="button" class="ghost" @click="deviceEditorOpen = false">{{ t('app.close') }}</button>
            <button type="submit" class="primary">{{ t('app.save') }}</button>
          </div>
        </form>
      </section>
    </div>
  </div>
</template>
