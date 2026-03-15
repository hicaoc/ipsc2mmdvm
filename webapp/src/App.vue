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
const accountCreatorOpen = ref(false)
const editingDeviceId = ref(null)
const pendingDevice = ref(null)
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
const audioAvailable = ref(false)
const audioError = ref('')
const selectedAudioTargets = ref([])
const deviceSearch = ref('')

let snapshotIntervalId = null
let durationIntervalId = null
let ws = null
let wsReconnectTimer = null
let audioContext = null
let audioMasterGain = null
let mixedAudioNextTime = 0
const audioStreams = new Map()
const authSessionHintKey = 'auth_session_hint'
const aLawFloatTable = buildALawFloatTable()

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
const filteredDevices = computed(() => filterDevices(sortedDevices.value, deviceSearch.value))
const filteredMyDevices = computed(() => filterDevices(myDevices.value, deviceSearch.value))
const editingDevice = computed(() =>
  pendingDevice.value?.id === editingDeviceId.value
    ? pendingDevice.value
    : sortedDevices.value.find((device) => device.id === editingDeviceId.value) || null
)
const sortedCalls = computed(() =>
  [...snapshot.value.calls].sort((a, b) => {
    const timeDiff = new Date(b.createdAt || 0).getTime() - new Date(a.createdAt || 0).getTime()
    if (timeDiff !== 0) return timeDiff
    return Number(b.id || 0) - Number(a.id || 0)
  })
)
const activeCalls = computed(() => sortedCalls.value.filter((call) => isActiveCall(call)))
const liveHeadlineCall = computed(() => activeCalls.value[0] || null)
const overviewCalls = computed(() => sortedCalls.value)
const audioTargets = computed(() => {
  const seen = new Set()
  const items = []
  for (const call of [...activeCalls.value, ...sortedCalls.value]) {
    const key = audioTargetKey(call)
    if (!key || seen.has(key)) continue
    seen.add(key)
    items.push({
      key,
      label: audioTargetLabel(call),
      active: isActiveCall(call)
    })
    if (items.length >= 8) break
  }
  return items
})
const audioEnabled = computed(() => selectedAudioTargets.value.length > 0)
const audioSubscriptionCount = computed(() => selectedAudioTargets.value.length)
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

function callRegisteredDeviceID(call) {
  const sourceKey = String(call?.sourceKey || '').trim()
  if (sourceKey) {
    const device = snapshot.value.devices.find((item) => item?.sourceKey === sourceKey)
    if (device?.dmrid) return String(device.dmrid)
  }
  return '-'
}

function callPacketDeviceID(call) {
  if (call?.repeaterId) return String(call.repeaterId)
  return '-'
}

function deviceAddress(device) {
  if (!device?.ip) return '-'
  return device.port ? `${device.ip}:${device.port}` : device.ip
}

function splitHostPort(value) {
  const raw = String(value || '').trim()
  if (!raw || raw === '-') return { host: '-', port: '-' }
  const withoutProtocol = raw.replace(/^[a-z]+:\/\//i, '')
  const withoutPath = withoutProtocol.split('/')[0]
  const lastColon = withoutPath.lastIndexOf(':')
  if (lastColon <= 0 || lastColon === withoutPath.length - 1) {
    return { host: withoutPath || '-', port: '-' }
  }
  return {
    host: withoutPath.slice(0, lastColon) || '-',
    port: withoutPath.slice(lastColon + 1) || '-'
  }
}

function runtimeHost(value) {
  return splitHostPort(value).host
}

function runtimePort(value) {
  return splitHostPort(value).port
}

const runtimePrimaryHost = computed(() => {
  const candidates = [
    snapshot.value.runtime?.ipscListen,
    snapshot.value.runtime?.hyteraP2pListen,
    snapshot.value.runtime?.hyteraDmrListen,
    snapshot.value.runtime?.hyteraRdacListen
  ]
  for (const item of candidates) {
    const host = runtimeHost(item)
    if (host && host !== '-') return host
  }
  return '-'
})

function filterDevices(devices, query) {
  const keyword = String(query || '').trim().toLowerCase()
  if (!keyword) return devices
  return devices.filter((device) => {
    const fields = [
      device?.name,
      device?.sourceKey,
      device?.callsign,
      device?.dmrid ? String(device.dmrid) : ''
    ]
    return fields.some((value) => String(value || '').toLowerCase().includes(keyword))
  })
}

function callTypeLabel(call) {
  if (call?.callType === 'private') return t('call.private')
  if (call?.callType === 'analog') return t('call.analog')
  return t('call.group')
}

function audioTargetKey(call) {
  if (!call) return ''
  if (call.callType === 'analog') {
    const sourceKey = String(call.sourceKey || '').replace(/^nrl:/, '')
    return sourceKey ? `analog:${sourceKey}` : ''
  }
  if (!call.dstId) return ''
  if (call.callType === 'private') return `private:${call.dstId}`
  return `group:${call.dstId}`
}

function audioTargetLabel(call) {
  if (!call) return '-'
  if (call.callType === 'analog') return `${t('call.analog')} · ${callSourceDevice(call)}`
  const id = call.dstId || '-'
  if (call.callType === 'private') return `${t('call.private')} ${id}`
  return `TG ${id}`
}

function isAudioTargetSelected(targetKey) {
  return selectedAudioTargets.value.includes(targetKey)
}

function audioTargetIcon(targetKey) {
  return isAudioTargetSelected(targetKey) ? '🔊' : '🔇'
}

function callStatusLabel(call) {
  return isActiveCall(call) ? t('call.live') : callTypeLabel(call)
}

function deviceStatusLabel(device) {
  if (device?.disabled) return t('app.disable')
  return device?.online ? t('call.online') : t('call.offline')
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

function deviceStaticGroupRows(device) {
  if (isNRLVirtualDevice(device)) {
    const slot = Number(device?.nrlSlot || 1) === 2 ? 2 : 1
    return [{ slot, value: groupSummary(device.sourceKey, slot, 'static') }]
  }
  return [
    { slot: 1, value: groupSummary(device.sourceKey, 1, 'static') },
    { slot: 2, value: groupSummary(device.sourceKey, 2, 'static') }
  ]
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

function isNRLVirtualDevice(device) {
  return String(device?.protocol || '').toLowerCase() === 'nrl-virtual'
}

function isManagedMMDVMDevice(device) {
  return String(device?.protocol || '').toLowerCase() === 'mmdvm-upstream'
}

function deviceToneClass(device) {
  if (isNRLVirtualDevice(device)) return 'device-tone-nrl'
  if (isManagedMMDVMDevice(device)) return 'device-tone-mmdvm'
  if (isHyteraDevice(device)) return 'device-tone-hytera'
  if (String(device?.protocol || '').toLowerCase() === 'ipsc') return 'device-tone-ipsc'
  if (String(device?.category || '').toLowerCase() === 'moto') return 'device-tone-ipsc'
  return 'device-tone-default'
}

function deviceExtra(device) {
  const raw = String(device?.extraJson || '').trim()
  if (!raw) return {}
  try {
    const parsed = JSON.parse(raw)
    return parsed && typeof parsed === 'object' ? parsed : {}
  } catch {
    return {}
  }
}

function deviceSecondaryInfo(device) {
  const items = []
  if (isAdmin.value) {
    const address = isHyteraDevice(device) ? (device?.ip || '-') : deviceAddress(device)
    items.push(`${t('device.address')} ${address}`)
  }
  if (isHyteraDevice(device)) {
    const extra = deviceExtra(device)
    items.push(`Hytera P2P ${extra?.p2pPort || '-'}`)
    items.push(`Hytera DMR ${extra?.dmrPort || device?.port || '-'}`)
    items.push(`Hytera RDAC ${extra?.rdacPort || '-'}`)
    items.push(`${t('device.nrlServerAddr')} ${device?.nrlServerAddr || '-'}`)
    items.push(`${t('device.nrlServerPort')} ${device?.nrlServerPort || '-'}`)
    items.push(`${t('device.nrlSsid')} ${device?.nrlSsid || '-'}`)
  }
  if (isNRLVirtualDevice(device)) {
    items.push(`${t('device.localEndpoint')} ${device?.ip || '-'}${device?.port ? `:${device.port}` : ''}`)
    items.push(`${t('device.nrlServerAddr')} ${device?.nrlServerAddr || '-'}`)
    items.push(`${t('device.nrlServerPort')} ${device?.nrlServerPort || '-'}`)
    items.push(`${t('device.nrlSsid')} ${device?.nrlSsid || '-'}`)
    items.push(`${t('device.nrlSlot')} ${device?.nrlSlot || 1}`)
  }
  if (isManagedMMDVMDevice(device)) {
    const extra = deviceExtra(device)
    const master = extra?.masterServer || deviceAddress(device)
    items.push(`${t('device.masterServer')} ${master || '-'}`)
    items.push(`${t('device.slotsLabel')} ${formatSlotMask(device?.slots || 3)}`)
  }
  return items
}

function formatSlotMask(mask) {
  const value = Number(mask || 0)
  const items = []
  if (value & 1) items.push('TS1')
  if (value & 2) items.push('TS2')
  return items.length ? items.join(' / ') : '-'
}

function normalizeRewriteRows(rows, keys) {
  if (!Array.isArray(rows)) return []
  return rows.map((row) => {
    const next = {}
    for (const key of keys) {
      next[key] = Number(row?.[key] || (key === 'range' ? 1 : 0))
    }
    return next
  })
}

function passAllSlots(maskLike) {
  const values = Array.isArray(maskLike) ? maskLike : []
  return values.map((item) => Number(item)).filter((item, index, list) => (item === 1 || item === 2) && list.indexOf(item) === index).sort()
}

function slotMaskFromArray(values) {
  let mask = 0
  for (const value of passAllSlots(values)) {
    mask |= value
  }
  return mask || 0
}

function makeDeviceDraft(device) {
  const extra = deviceExtra(device)
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
    nrlSlot: device.nrlSlot || 1,
    mmdvmMasterServer: extra.masterServer || (device.ip ? (device.port ? `${device.ip}:${device.port}` : device.ip) : ''),
    rxFreq: device.rxFreq || '',
    txFreq: device.txFreq || '',
    txPower: device.txPower || '',
    colorCode: device.colorCode || '',
    latitude: device.latitude || '',
    longitude: device.longitude || '',
    height: device.height || '',
    url: device.url || '',
    mmdvmSlots: passAllSlots([((Number(device.slots || 3) & 1) ? 1 : 0), ((Number(device.slots || 3) & 2) ? 2 : 0)]),
    tgRewrites: normalizeRewriteRows(extra.tgRewrites, ['fromSlot', 'fromTG', 'toSlot', 'toTG', 'range']),
    pcRewrites: normalizeRewriteRows(extra.pcRewrites, ['fromSlot', 'fromId', 'toSlot', 'toId', 'range']),
    typeRewrites: normalizeRewriteRows(extra.typeRewrites, ['fromSlot', 'fromTG', 'toSlot', 'toId', 'range']),
    srcRewrites: normalizeRewriteRows(extra.srcRewrites, ['fromSlot', 'fromId', 'toSlot', 'toId', 'range']),
    passAllTG: passAllSlots(extra.passAllTG),
    passAllPC: passAllSlots(extra.passAllPC),
    staticSlot1: slotSubscriptions(device.sourceKey, 1).filter((item) => item.kind === 'static').map((item) => item.groupId).join(','),
    staticSlot2: slotSubscriptions(device.sourceKey, 2).filter((item) => item.kind === 'static').map((item) => item.groupId).join(',')
  }
}

function rebuildDeviceDrafts() {
  const activeEditId = deviceEditorOpen.value ? editingDeviceId.value : null
  const currentDrafts = deviceDrafts.value
  const nextDrafts = Object.fromEntries(snapshot.value.devices.map((device) => {
    if (activeEditId === device.id && currentDrafts[device.id]) {
      return [device.id, currentDrafts[device.id]]
    }
    return [device.id, makeDeviceDraft(device)]
  }))
  if (pendingDevice.value) {
    nextDrafts[pendingDevice.value.id] = currentDrafts[pendingDevice.value.id] || makeDeviceDraft(pendingDevice.value)
  }
  deviceDrafts.value = nextDrafts
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

function updateRuntime(runtime) {
  snapshot.value = {
    ...snapshot.value,
    runtime: {
      ...(snapshot.value.runtime || {}),
      ...(runtime || {})
    }
  }
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
    closeDeviceEditor()
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

async function ensureAudioContext() {
  try {
    const AudioCtor = window.AudioContext || window.webkitAudioContext
    if (!AudioCtor) throw new Error(t('audio.unsupported'))
    if (!audioContext) {
      audioContext = new AudioCtor()
      audioMasterGain = audioContext.createGain()
      audioMasterGain.gain.value = 1
      audioMasterGain.connect(audioContext.destination)
    }
    await audioContext.resume()
    audioError.value = ''
    return true
  } catch (error) {
    selectedAudioTargets.value = []
    audioError.value = error?.message || t('audio.enableFailed')
    return false
  }
}

async function disableAudio() {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'audio_unsubscribe' }))
  }
  audioAvailable.value = false
  selectedAudioTargets.value = []
  audioStreams.clear()
  mixedAudioNextTime = 0
  if (audioContext) {
    try {
      await audioContext.close()
    } catch {}
  }
  audioContext = null
  audioMasterGain = null
}

function chunkTargetKey(payload) {
  if (!payload) return ''
  if (payload.callType === 'analog') {
    const sourceKey = String(payload.sourceKey || '').trim()
    return sourceKey ? `analog:${sourceKey}` : ''
  }
  const dstId = Number(payload.dstId || 0)
  if (!dstId) return ''
  if (payload.callType === 'private') return `private:${dstId}`
  return `group:${dstId}`
}

function cleanupAudioStreams(now = performance.now()) {
  for (const [streamId, state] of audioStreams.entries()) {
    if (state.targetKey && !isAudioTargetSelected(state.targetKey)) {
      audioStreams.delete(streamId)
      continue
    }
    if (state.ended && state.nextTime <= (audioContext?.currentTime || 0) + 0.05) {
      audioStreams.delete(streamId)
      continue
    }
    if (now - state.lastSeenAt > 5000) {
      audioStreams.delete(streamId)
    }
  }
}

async function toggleAudioTarget(targetKey) {
  const nextTarget = String(targetKey || '').trim()
  if (!nextTarget) return
  if (isAudioTargetSelected(nextTarget)) {
    const remaining = selectedAudioTargets.value.filter((item) => item !== nextTarget)
    if (!remaining.length) {
      await disableAudio()
      return
    }
    selectedAudioTargets.value = remaining
    cleanupAudioStreams()
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify({ type: 'audio_unsubscribe', target: nextTarget }))
    }
    return
  }
  const ready = await ensureAudioContext()
  if (!ready) return
  selectedAudioTargets.value = [...selectedAudioTargets.value, nextTarget]
  audioAvailable.value = false
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: 'audio_subscribe', target: nextTarget }))
  }
}

function decodePCM16Base64(value) {
  const raw = window.atob(String(value || ''))
  const bytes = new Uint8Array(raw.length)
  for (let i = 0; i < raw.length; i += 1) bytes[i] = raw.charCodeAt(i)
  const view = new DataView(bytes.buffer)
  const out = new Float32Array(bytes.length / 2)
  for (let i = 0; i < out.length; i += 1) {
    out[i] = view.getInt16(i * 2, true) / 32768
  }
  return out
}

function decodePCM16Bytes(buffer) {
  const view = new DataView(buffer)
  const out = new Float32Array(Math.floor(buffer.byteLength / 2))
  for (let i = 0; i < out.length; i += 1) {
    out[i] = view.getInt16(i * 2, true) / 32768
  }
  return out
}

function decodeALawBytes(buffer) {
  const bytes = new Uint8Array(buffer)
  const out = new Float32Array(bytes.length)
  for (let i = 0; i < bytes.length; i += 1) {
    out[i] = aLawFloatTable[bytes[i]]
  }
  return out
}

function buildALawFloatTable() {
  const table = new Float32Array(256)
  for (let i = 0; i < 256; i += 1) {
    let value = i ^ 0x55
    let exponent = (value & 0x70) >> 4
    let mantissa = value & 0x0f
    if (exponent > 0) mantissa += 16
    let sample = (mantissa << 4) + 0x08
    if (exponent > 1) sample <<= (exponent - 1)
    table[i] = ((value & 0x80) !== 0 ? sample : -sample) / 32768
  }
  return table
}

function schedulePCMPlayback(pcm, sampleRate, nextTimeRef = null) {
  if (!pcm.length || !audioContext || !audioMasterGain) return

  const buffer = audioContext.createBuffer(1, pcm.length, sampleRate)
  buffer.getChannelData(0).set(pcm)

  const source = audioContext.createBufferSource()
  source.buffer = buffer
  source.connect(audioMasterGain)

  const leadTime = 0.02
  const now = audioContext.currentTime
  let queued = nextTimeRef && nextTimeRef.nextTime > now ? nextTimeRef.nextTime : now + leadTime
  if (queued - now > 0.4) queued = now + leadTime
  source.start(queued)

  if (nextTimeRef) {
    nextTimeRef.nextTime = queued + buffer.duration
  } else {
    mixedAudioNextTime = queued + buffer.duration
  }
}

function handleMixedAudioFrame(buffer) {
  audioAvailable.value = true
  if (!audioEnabled.value || !audioContext || !audioMasterGain) return

  const pcm = decodeALawBytes(buffer)
  if (!pcm.length) return

  schedulePCMPlayback(pcm, 8000, { get nextTime() { return mixedAudioNextTime }, set nextTime(value) { mixedAudioNextTime = value } })
}

function handleAudioChunk(payload) {
  audioAvailable.value = true
  if (!payload?.streamId) return
  const targetKey = chunkTargetKey(payload)
  if (targetKey && !isAudioTargetSelected(targetKey)) return
  const state = audioStreams.get(payload.streamId) || { nextTime: 0, lastSeenAt: 0, ended: false, targetKey: '' }
  state.lastSeenAt = performance.now()
  state.ended = Boolean(payload.ended)
  state.targetKey = targetKey
  audioStreams.set(payload.streamId, state)
  cleanupAudioStreams(state.lastSeenAt)

  if (!audioEnabled.value || !payload.pcm || !audioContext || !audioMasterGain) return

  const pcm = decodePCM16Base64(payload.pcm)
  if (!pcm.length) return
  schedulePCMPlayback(pcm, Number(payload.sampleRate || 8000), state)
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
    ws.binaryType = 'arraybuffer'
  } catch {
    ws = null
    scheduleReconnect()
    return
  }
  ws.onopen = () => {
    wsConnected.value = true
    for (const targetKey of selectedAudioTargets.value) {
      ws.send(JSON.stringify({ type: 'audio_subscribe', target: targetKey }))
    }
  }
  ws.onmessage = (event) => {
    if (event.data instanceof ArrayBuffer) {
      handleMixedAudioFrame(event.data)
      return
    }
    const payload = JSON.parse(event.data)
    if (payload.type === 'snapshot' && payload.snapshot) applySnapshot(payload.snapshot)
    if (payload.type === 'device_updated' && payload.device) upsertDevice(payload.device)
    if (payload.type === 'device_deleted' && payload.device) removeDevice(payload.device)
    if (payload.type === 'call_recorded' && payload.call) appendCall(payload.call)
    if (payload.type === 'runtime_updated' && payload.runtime) updateRuntime(payload.runtime)
    if (payload.type === 'audio_chunk') handleAudioChunk(payload)
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
  const nrlVirtualDevice = isNRLVirtualDevice(device)
  const mmdvmUpstreamDevice = isManagedMMDVMDevice(device)
  const nrlSlot = Number(draft.nrlSlot || 1) === 2 ? 2 : 1
  const sharedNRLCallsign = normalizeCallsign(draft.callsign || device.callsign || user.value?.callsign || '')
  const normalizedCallsign = nrlVirtualDevice ? sharedNRLCallsign : normalizeCallsign(draft.callsign)
  if (mmdvmUpstreamDevice || String(device.id).startsWith('new-mmdvm-')) {
    const payload = {
      protocol: 'mmdvm-upstream',
      name: draft.name,
      callsign: draft.callsign,
      dmrid: Number(draft.dmrid || 0),
      description: draft.description,
      location: draft.location,
      notes: draft.notes,
      devicePassword: draft.devicePassword,
      mmdvmMasterServer: draft.mmdvmMasterServer,
      rxFreq: Number(draft.rxFreq || 0),
      txFreq: Number(draft.txFreq || 0),
      txPower: Number(draft.txPower || 0),
      colorCode: Number(draft.colorCode || 0),
      latitude: Number(draft.latitude || 0),
      longitude: Number(draft.longitude || 0),
      height: Number(draft.height || 0),
      url: draft.url,
      slots: slotMaskFromArray(draft.mmdvmSlots),
      tgRewrites: normalizeRewriteRows(draft.tgRewrites, ['fromSlot', 'fromTG', 'toSlot', 'toTG', 'range']),
      pcRewrites: normalizeRewriteRows(draft.pcRewrites, ['fromSlot', 'fromId', 'toSlot', 'toId', 'range']),
      typeRewrites: normalizeRewriteRows(draft.typeRewrites, ['fromSlot', 'fromTG', 'toSlot', 'toId', 'range']),
      srcRewrites: normalizeRewriteRows(draft.srcRewrites, ['fromSlot', 'fromId', 'toSlot', 'toId', 'range']),
      passAllTG: passAllSlots(draft.passAllTG),
      passAllPC: passAllSlots(draft.passAllPC)
    }
    if (String(device.id).startsWith('new-mmdvm-')) {
      await api.createDevice(payload)
      delete deviceDrafts.value[device.id]
      pendingDevice.value = null
    } else {
      await api.updateDevice(device.id, payload)
    }
    message.value = t('device.saveSuccess')
    closeDeviceEditor()
    await loadSnapshot()
    return
  }
  const staticSlot1 = nrlVirtualDevice ? (nrlSlot === 1 ? primaryGroup(draft.staticSlot1) : []) : parseGroups(draft.staticSlot1)
  const staticSlot2 = nrlVirtualDevice ? (nrlSlot === 2 ? primaryGroup(draft.staticSlot2) : []) : parseGroups(draft.staticSlot2)
  const payload = {
    name: draft.name,
    notes: draft.notes,
    description: draft.description,
    staticSlot1,
    staticSlot2
  }
  if (!nrlVirtualDevice) {
    payload.model = draft.model
    payload.location = draft.location
    payload.devicePassword = draft.devicePassword
  }
  if (hyteraDevice || nrlVirtualDevice) {
    payload.nrlServerAddr = draft.nrlServerAddr
    payload.nrlServerPort = Number(draft.nrlServerPort || 0)
    payload.nrlSsid = Number(draft.nrlSsid || 0)
    payload.nrlSlot = nrlSlot
  }
  if (isAdmin.value) {
    payload.callsign = normalizedCallsign
    if (!nrlVirtualDevice) {
      payload.dmrid = Number(draft.dmrid || 0)
    }
  }
  if (nrlVirtualDevice && !normalizedCallsign) {
    throw new Error(t('device.nrlCallsignRequired'))
  }
  if (String(device.id).startsWith('new-nrl-')) {
    await api.createDevice({
      name: draft.name,
      callsign: normalizedCallsign,
      description: draft.description,
      notes: draft.notes,
      nrlServerAddr: draft.nrlServerAddr,
      nrlServerPort: Number(draft.nrlServerPort || 0),
      nrlSsid: Number(draft.nrlSsid || 0),
      nrlSlot,
      staticGroups: nrlSlot === 1 ? staticSlot1 : staticSlot2
    })
    delete deviceDrafts.value[device.id]
    pendingDevice.value = null
  } else {
    await api.updateDevice(device.id, payload)
  }
  message.value = t('device.saveSuccess')
  closeDeviceEditor()
  await loadSnapshot()
}

async function createNRLVirtualDevice() {
  if (!isAdmin.value) return
  const device = {
    id: `new-nrl-${Date.now()}`,
    protocol: 'nrl-virtual',
    sourceKey: '',
    name: '',
    callsign: user.value?.callsign || '',
    dmrid: 0,
    model: '',
    description: '',
    location: '',
    notes: '',
    devicePassword: '',
    nrlServerAddr: '',
    nrlServerPort: 0,
    nrlSsid: 0,
    nrlSlot: 1
  }
  pendingDevice.value = device
  deviceDrafts.value[device.id] = makeDeviceDraft(device)
  openDeviceEditor(device)
}

async function createMMDVMUpstreamDevice() {
  if (!isAdmin.value) return
  const device = {
    id: `new-mmdvm-${Date.now()}`,
    protocol: 'mmdvm-upstream',
    sourceKey: '',
    name: '',
    callsign: '',
    dmrid: 0,
    model: 'MMDVM Master',
    description: '',
    location: '',
    notes: '',
    devicePassword: '',
    rxFreq: 0,
    txFreq: 0,
    txPower: 0,
    colorCode: 1,
    latitude: 0,
    longitude: 0,
    height: 0,
    url: '',
    slots: 3,
    extraJson: ''
  }
  pendingDevice.value = device
  deviceDrafts.value[device.id] = makeDeviceDraft(device)
  openDeviceEditor(device)
}

async function deleteDevice(device) {
  if (!isAdmin.value) return
  if (!window.confirm(`${t('app.delete')} ${device.name || device.callsign || device.sourceKey}?`)) return
  await api.deleteDevice(device.id)
  if (editingDeviceId.value === device.id) {
    closeDeviceEditor()
  }
}

async function toggleDeviceEnabled(device) {
  if (!canEditDevice(device) && !isAdmin.value) return
  try {
    message.value = ''
    await api.updateDevice(device.id, { disabled: !device.disabled })
    await loadSnapshot()
  } catch (error) {
    message.value = error.message || 'Failed to update device'
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
  pendingDevice.value = String(device?.id || '').startsWith('new-nrl-') ? device : null
  if (String(device?.id || '').startsWith('new-mmdvm-')) pendingDevice.value = device
  editingDeviceId.value = device.id
  deviceEditorOpen.value = true
}

function closeDeviceEditor() {
  if (pendingDevice.value) {
    delete deviceDrafts.value[pendingDevice.value.id]
    pendingDevice.value = null
  }
  editingDeviceId.value = null
  deviceEditorOpen.value = false
}

function openAccountCreator() {
  accountCreatorOpen.value = true
}

function parseGroups(value) {
  return String(value || '')
    .split(/[\s,]+/)
    .map((item) => Number(item.trim()))
    .filter((item, index, list) => Number.isFinite(item) && item > 0 && list.indexOf(item) === index)
}

function primaryGroup(value) {
  const groups = parseGroups(value)
  return groups.length ? groups.slice(0, 1) : []
}

function newRewriteRow(kind) {
  if (kind === 'tg') return { fromSlot: 1, fromTG: 0, toSlot: 1, toTG: 0, range: 1 }
  if (kind === 'pc') return { fromSlot: 1, fromId: 0, toSlot: 1, toId: 0, range: 1 }
  if (kind === 'type') return { fromSlot: 1, fromTG: 0, toSlot: 1, toId: 0, range: 1 }
  return { fromSlot: 1, fromId: 0, toSlot: 1, toId: 0, range: 1 }
}

function addRewriteRule(deviceId, key, kind) {
  const draft = deviceDrafts.value[deviceId]
  if (!draft) return
  draft[key].push(newRewriteRow(kind))
}

function removeRewriteRule(deviceId, key, index) {
  const draft = deviceDrafts.value[deviceId]
  if (!draft) return
  draft[key].splice(index, 1)
}

onMounted(async () => {
  await Promise.all([loadSession(), loadSnapshot()])
  connectWS()
  durationIntervalId = window.setInterval(() => {
    nowTick.value = Date.now()
    cleanupAudioStreams()
  }, 1000)
  snapshotIntervalId = window.setInterval(() => {
    loadSnapshot().catch(() => {})
  }, 10000)
})

onUnmounted(() => {
  if (durationIntervalId) window.clearInterval(durationIntervalId)
  if (snapshotIntervalId) window.clearInterval(snapshotIntervalId)
  closeWS()
  disableAudio()
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

    <section v-if="audioError" class="glass audio-error-banner">
      {{ audioError }}
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
      <article class="metric-card glass">
        <span>{{ t('dashboard.browserClients') }}</span>
        <strong>{{ snapshot.runtime?.browserClients || 0 }}</strong>
      </article>
      <article class="metric-card glass">
        <span>{{ t('dashboard.audioSubscribers') }}</span>
        <strong>{{ snapshot.runtime?.audioSubscribers || 0 }}</strong>
      </article>
    </section>

    <section class="workspace">
      <aside class="sidebar glass">
        <div class="sidebar-head">
          <p class="eyebrow">{{ t('dashboard.welcome') }}</p>
          <strong v-if="user">{{ `${user.username} / ${user.callsign || '-'}` }}</strong>
          <span class="muted-inline">{{ wsConnected ? t('call.wsOnline') : t('call.wsOffline') }}</span>
        </div>
        <nav class="sidebar-nav" :aria-label="t('app.overview')">
          <button v-for="item in navItems" :key="item.key" :class="{ active: activePanel === item.key }" @click="activePanel = item.key">
            {{ item.label }}
          </button>
        </nav>
        <article class="glass inset runtime-card runtime-panel runtime-panel-sidebar">
          <div class="section-head runtime-panel-head">
            <h3>{{ t('dashboard.systemInfo') }}</h3>
          </div>
          <div class="kv-list compact runtime-kv-list runtime-kv-list-sidebar">
            <div class="kv-row"><span>IP</span><strong>{{ runtimePrimaryHost }}</strong></div>
            <div class="kv-row"><span>Moto IPSC</span><strong>{{ runtimePort(snapshot.runtime.ipscListen) }}</strong></div>
            <div class="kv-row"><span>Hytera P2P</span><strong>{{ runtimePort(snapshot.runtime.hyteraP2pListen) }}</strong></div>
            <div class="kv-row"><span>Hytera DMR</span><strong>{{ runtimePort(snapshot.runtime.hyteraDmrListen) }}</strong></div>
            <div class="kv-row"><span>Hytera RDAC</span><strong>{{ runtimePort(snapshot.runtime.hyteraRdacListen) }}</strong></div>
          </div>
        </article>
      </aside>

      <main
        :class="[
          'content',
          {
            'content-shell': activePanel !== 'overview',
            'content-scroll': ['devices', 'accounts', 'my-devices'].includes(activePanel),
            'content-overview': activePanel === 'overview'
          }
        ]"
      >
        <template v-if="activePanel === 'overview'">
          <article class="glass inset runtime-card call-focus-panel">
            <div class="call-panel-head">
              <div>
                <h3>{{ t('app.calls') }}</h3>
                <p class="hint">{{ t('call.priorityHint') }}</p>
              </div>
              <div class="call-panel-actions audio-target-panel">
                <div v-if="audioTargets.length" class="audio-target-list">
                  <button
                    v-for="target in audioTargets"
                    :key="target.key"
                    :class="isAudioTargetSelected(target.key) ? 'primary' : 'ghost'"
                    @click="toggleAudioTarget(target.key)"
                  >
                    <span class="audio-target-icon">{{ audioTargetIcon(target.key) }}</span>
                    {{ target.label }}
                    <span v-if="target.active" class="muted-inline">· {{ t('call.live') }}</span>
                  </button>
                </div>
                <span v-else class="muted-inline">{{ t('audio.noTargets') }}</span>
                <span class="call-count-badge">{{ audioSubscriptionCount }} {{ t('audio.streams') }}</span>
                <span class="call-count-badge">{{ activeCalls.length }} {{ t('call.live') }}</span>
              </div>
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
                <div class="call-card-compact-head">
                  <div class="call-primary">
                    <span v-if="isActiveCall(call)" class="pulse-dot"></span>
                    <strong>{{ callTitle(call) }}</strong>
                  </div>
                  <div class="call-card-compact-side">
                    <span :class="['call-status-badge', { live: isActiveCall(call) }]">{{ callStatusLabel(call) }}</span>
                    <strong class="call-card-compact-duration">{{ fmtDuration(call) }}</strong>
                  </div>
                </div>
                <div class="call-card-compact-layout">
                  <div class="call-card-compact-grid">
                    <div class="call-pill"><span>TG</span><strong>{{ call.dstId || '-' }}</strong></div>
                    <div class="call-pill"><span>TS</span><strong>{{ call.slot || '-' }}</strong></div>
                  </div>
                  <div class="call-card-compact-grid call-card-compact-grid-secondary">
                    <div class="call-pill call-pill-dmrid"><span>{{ t('call.dmrid') }}</span><strong>{{ call.sourceDmrid || '-' }}</strong></div>
                    <div class="call-pill call-pill-started"><span>{{ t('call.startedAt') }}</span><strong>{{ fmtTime(call.createdAt) }}</strong></div>
                  </div>
                </div>
                <div class="call-card-compact-footer">
                  <span class="muted-inline">{{ t('call.sourceDevice') }} {{ callSourceDevice(call) }}</span>
                  <span class="muted-inline">设备ID {{ callRegisteredDeviceID(call) }}</span>
                  <span class="muted-inline">报文ID {{ callPacketDeviceID(call) }}</span>
                  <span class="muted-inline">{{ call.frontend || '-' }}</span>
                  <span class="muted-inline">{{ call.fromIp || '-' }}{{ call.fromPort ? `:${call.fromPort}` : '' }}</span>
                </div>
              </article>
              <div v-if="!overviewCalls.length" class="hint">{{ t('call.empty') }}</div>
            </div>
          </article>
        </template>

        <template v-else-if="activePanel === 'devices'">
          <div class="section-head">
            <h2>{{ t('app.devices') }}</h2>
            <div class="section-head-side">
              <div class="section-search-wrap">
                <input v-model="deviceSearch" class="search-input" :placeholder="t('device.searchPlaceholder')" />
              </div>
              <div v-if="isAdmin" class="section-action-group">
                <button class="primary" @click="createNRLVirtualDevice">{{ t('device.createNrlVirtual') }}</button>
                <button class="ghost" @click="createMMDVMUpstreamDevice">{{ t('device.createMmdvmClient') }}</button>
              </div>
            </div>
          </div>
          <div class="device-card-list device-list-grid">
            <article v-for="device in filteredDevices" :key="device.id" :class="['glass', 'inset', 'device-list-card', deviceToneClass(device), { 'device-card-disabled': device.disabled }]">
              <div class="device-mobile-head device-list-head">
                <div>
                  <strong>{{ device.name || device.sourceKey }}</strong>
                  <p class="muted-inline">
                    {{ device.callsign || '-' }} · {{ isNRLVirtualDevice(device) ? (device.protocol || '-') : `${device.dmrid || '-'} · ${device.protocol || '-'}` }}
                  </p>
                </div>
                <div class="device-head-side">
                  <span :class="['table-status', { online: device.online && !device.disabled, disabled: device.disabled }]">{{ deviceStatusLabel(device) }}</span>
                  <div v-if="canEditDevice(device) || isAdmin" class="account-actions device-actions-inline">
                    <button v-if="canEditDevice(device)" class="primary" @click="openDeviceEditor(device)">{{ t('app.edit') }}</button>
                    <button v-if="canEditDevice(device) || isAdmin" class="ghost" @click="toggleDeviceEnabled(device)">{{ device.disabled ? t('app.enable') : t('app.disable') }}</button>
                    <button v-if="isAdmin" class="ghost danger" @click="deleteDevice(device)">{{ t('app.delete') }}</button>
                  </div>
                </div>
              </div>
              <div class="device-info-grid">
                <div v-for="row in deviceStaticGroupRows(device)" :key="`static-${device.id}-${row.slot}`" class="kv-row"><span>TS{{ row.slot }} {{ t('device.staticGroups') }}</span><strong>{{ row.value }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>TS1 {{ t('device.dynamicGroups') }}</span><strong>{{ groupSummary(device.sourceKey, 1, 'dynamic') }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>TS2 {{ t('device.dynamicGroups') }}</span><strong>{{ groupSummary(device.sourceKey, 2, 'dynamic') }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.model') }}</span><strong>{{ device.model || '-' }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.location') }}</span><strong>{{ device.location || '-' }}</strong></div>
                <div class="kv-row"><span>{{ t('device.notes') }}</span><strong>{{ device.notes || '-' }}</strong></div>
              </div>
              <div class="device-card-footer">
                <span v-for="item in deviceSecondaryInfo(device)" :key="item" class="muted-inline">{{ item }}</span>
              </div>
              <div v-if="canEditDevice(device) || isAdmin" class="account-actions device-actions-bottom">
                <button v-if="canEditDevice(device)" class="primary" @click="openDeviceEditor(device)">{{ t('app.edit') }}</button>
                <button v-if="canEditDevice(device) || isAdmin" class="ghost" @click="toggleDeviceEnabled(device)">{{ device.disabled ? t('app.enable') : t('app.disable') }}</button>
                <button v-if="isAdmin" class="ghost danger" @click="deleteDevice(device)">{{ t('app.delete') }}</button>
              </div>
            </article>
            <div v-if="!filteredDevices.length" class="hint">{{ t('device.noSearchResults') }}</div>
          </div>
        </template>

        <template v-else-if="activePanel === 'my-devices'">
          <div class="section-head">
            <h2>{{ t('app.myDevices') }}</h2>
            <div class="section-head-side">
              <input v-model="deviceSearch" class="search-input" :placeholder="t('device.searchPlaceholder')" />
              <span class="muted-inline">{{ t('device.myDevicesHint') }}</span>
            </div>
          </div>
          <div class="my-device-list">
            <article v-for="device in filteredMyDevices" :key="device.id" :class="['glass', 'inset', 'my-device-card', deviceToneClass(device), { 'device-card-disabled': device.disabled }]">
              <div class="my-device-head">
                <div>
                  <strong>{{ device.name || device.sourceKey }}</strong>
                  <p class="muted-inline">
                    {{ device.callsign || '-' }} · {{ isNRLVirtualDevice(device) ? (device.protocol || '-') : `${device.dmrid || '-'} · ${device.protocol || '-'}` }}
                  </p>
                </div>
                <span :class="['table-status', { online: device.online && !device.disabled, disabled: device.disabled }]">{{ deviceStatusLabel(device) }}</span>
              </div>
              <div class="kv-list compact">
                <div class="kv-row"><span>{{ t('device.callsign') }}</span><strong>{{ device.callsign || '-' }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.dmrid') }}</span><strong>{{ device.dmrid || '-' }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.model') }}</span><strong>{{ device.model || '-' }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.location') }}</span><strong>{{ device.location || '-' }}</strong></div>
                <div v-if="!isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.password') }}</span><strong>{{ device.devicePassword || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device) || isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.nrlServerAddr') }}</span><strong>{{ device.nrlServerAddr || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device) || isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.nrlServerPort') }}</span><strong>{{ device.nrlServerPort || '-' }}</strong></div>
                <div v-if="isHyteraDevice(device) || isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.nrlSsid') }}</span><strong>{{ device.nrlSsid || '-' }}</strong></div>
                <div v-if="isNRLVirtualDevice(device)" class="kv-row"><span>{{ t('device.nrlSlot') }}</span><strong>{{ device.nrlSlot || 1 }}</strong></div>
                <div class="kv-row"><span>{{ t('device.notes') }}</span><strong>{{ device.notes || '-' }}</strong></div>
              </div>
              <div class="account-actions">
                <button v-if="canEditDevice(device)" class="primary" @click="openDeviceEditor(device)">{{ t('app.edit') }}</button>
                <button v-if="canEditDevice(device) || isAdmin" class="ghost" @click="toggleDeviceEnabled(device)">{{ device.disabled ? t('app.enable') : t('app.disable') }}</button>
                <button v-if="isAdmin" class="ghost danger" @click="deleteDevice(device)">{{ t('app.delete') }}</button>
              </div>
            </article>
            <div v-if="!myDevices.length" class="hint">{{ t('device.noOwnedDevices') }}</div>
            <div v-else-if="!filteredMyDevices.length" class="hint">{{ t('device.noSearchResults') }}</div>
          </div>
        </template>

        <template v-else>
          <div class="section-head">
            <h2>{{ t('app.accounts') }}</h2>
            <button class="primary" @click="openAccountCreator">{{ t('app.create') }}</button>
          </div>
          <div class="account-layout">
            <div class="account-list">
              <article v-for="account in users" :key="account.id" class="account-card">
                <div class="account-card-head">
                  <div>
                    <strong>{{ account.username }}</strong>
                    <p>{{ account.callsign }} · {{ account.email }}</p>
                  </div>
                  <span :class="['table-status', { online: account.enabled }]">{{ account.enabled ? t('app.enabled') : t('app.disable') }}</span>
                </div>
                <div class="account-meta-grid">
                  <div class="kv-row"><span>{{ t('auth.callsign') }}</span><strong>{{ account.callsign || '-' }}</strong></div>
                  <div class="kv-row"><span>{{ t('auth.email') }}</span><strong>{{ account.email || '-' }}</strong></div>
                  <div class="kv-row"><span>{{ t('auth.role') }}</span><strong>{{ roleLabel(account.role) }}</strong></div>
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
          <h2>{{ String(editingDevice.id).startsWith('new-nrl-') ? `${t('app.create')} · ${t('device.createNrlVirtual')}` : String(editingDevice.id).startsWith('new-mmdvm-') ? `${t('app.create')} · ${t('device.createMmdvmClient')}` : `${t('app.edit')} · ${editingDevice.name || editingDevice.callsign || editingDevice.sourceKey}` }}</h2>
          <button class="ghost" @click="closeDeviceEditor()">{{ t('app.close') }}</button>
        </div>
        <form class="form-grid device-editor-form" @submit.prevent="saveDevice(editingDevice)">
          <div class="device-editor-grid">
            <section class="device-editor-section field-span-2">
              <div class="device-editor-section-head">
                <h3>{{ t('app.devices') }}</h3>
              </div>
              <div class="device-editor-section-grid">
                <label class="field-block">
                  <span>{{ t('device.name') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].name" :placeholder="t('device.name')" />
                </label>
                <label v-if="!isNRLVirtualDevice(editingDevice) && !isManagedMMDVMDevice(editingDevice)" class="field-block">
                  <span>{{ t('device.model') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].model" :placeholder="t('device.model')" />
                </label>
                <label v-if="isAdmin" class="field-block">
                  <span>{{ t('device.callsign') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].callsign" :placeholder="t('device.callsign')" />
                </label>
                <label v-if="isAdmin && !isNRLVirtualDevice(editingDevice)" class="field-block">
                  <span>{{ t('device.dmrid') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].dmrid" :placeholder="t('device.dmrid')" />
                </label>
                <label v-if="!isNRLVirtualDevice(editingDevice)" class="field-block">
                  <span>{{ t('device.location') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].location" :placeholder="t('device.location')" />
                </label>
                <label v-if="!isNRLVirtualDevice(editingDevice)" class="field-block">
                  <span>{{ t('device.password') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].devicePassword" :placeholder="t('device.password')" />
                </label>
              </div>
            </section>

            <section v-if="!isManagedMMDVMDevice(editingDevice)" class="device-editor-section field-span-2">
              <div class="device-editor-section-head">
                <h3>{{ t('device.staticGroups') }}</h3>
              </div>
              <div class="device-editor-section-grid">
                <label class="field-block">
                  <span>{{ t('device.ts1') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].staticSlot1" :disabled="isNRLVirtualDevice(editingDevice) && Number(deviceDrafts[editingDevice.id].nrlSlot || 1) !== 1" :placeholder="t('device.ts1')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.ts2') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].staticSlot2" :disabled="isNRLVirtualDevice(editingDevice) && Number(deviceDrafts[editingDevice.id].nrlSlot || 1) !== 2" :placeholder="t('device.ts2')" />
                </label>
              </div>
            </section>

            <section v-if="isHyteraDevice(editingDevice) || isNRLVirtualDevice(editingDevice)" class="device-editor-section field-span-2 device-editor-section-nrl">
              <div class="device-editor-section-head">
                <h3>NRL</h3>
              </div>
              <div class="device-editor-section-grid">
                <label class="field-block">
                  <span>{{ t('device.nrlServerAddr') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].nrlServerAddr" :placeholder="t('device.nrlServerAddr')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.nrlServerPort') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].nrlServerPort" :placeholder="t('device.nrlServerPort')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.nrlSsid') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].nrlSsid" :placeholder="t('device.nrlSsid')" />
                </label>
                <fieldset v-if="isNRLVirtualDevice(editingDevice)" class="field-block slot-radio-group">
                  <span>{{ t('device.nrlSlot') }}</span>
                  <div class="slot-radio-options">
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].nrlSlot" type="radio" :value="1" />
                      <span>TS1</span>
                    </label>
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].nrlSlot" type="radio" :value="2" />
                      <span>TS2</span>
                    </label>
                  </div>
                </fieldset>
              </div>
            </section>

            <section v-if="isManagedMMDVMDevice(editingDevice)" class="device-editor-section field-span-2 device-editor-section-nrl">
              <div class="device-editor-section-head">
                <h3>MMDVM</h3>
              </div>
              <div class="device-editor-section-grid">
                <label class="field-block">
                  <span>{{ t('device.masterServer') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].mmdvmMasterServer" :placeholder="t('device.masterServer')" />
                </label>
                <fieldset class="field-block slot-radio-group">
                  <span>{{ t('device.slotsLabel') }}</span>
                  <div class="slot-radio-options">
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].mmdvmSlots" type="checkbox" :value="1" />
                      <span>TS1</span>
                    </label>
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].mmdvmSlots" type="checkbox" :value="2" />
                      <span>TS2</span>
                    </label>
                  </div>
                </fieldset>
                <label class="field-block">
                  <span>{{ t('device.rxFreq') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].rxFreq" :placeholder="t('device.rxFreq')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.txFreq') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].txFreq" :placeholder="t('device.txFreq')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.txPower') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].txPower" :placeholder="t('device.txPower')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.colorCode') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].colorCode" :placeholder="t('device.colorCode')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.latitude') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].latitude" :placeholder="t('device.latitude')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.longitude') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].longitude" :placeholder="t('device.longitude')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.height') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].height" :placeholder="t('device.height')" />
                </label>
                <label class="field-block">
                  <span>{{ t('device.url') }}</span>
                  <input v-model="deviceDrafts[editingDevice.id].url" :placeholder="t('device.url')" />
                </label>
              </div>
            </section>

            <section v-if="isManagedMMDVMDevice(editingDevice)" class="device-editor-section field-span-2">
              <div class="device-editor-section-head">
                <h3>{{ t('device.rewriteRules') }}</h3>
              </div>
              <div class="rewrite-groups">
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.tgRewrite') }}</strong>
                    <button type="button" class="ghost" @click="addRewriteRule(editingDevice.id, 'tgRewrites', 'tg')">{{ t('device.addRule') }}</button>
                  </div>
                  <div v-if="!deviceDrafts[editingDevice.id].tgRewrites.length" class="hint">{{ t('device.noRules') }}</div>
                  <div v-for="(rule, index) in deviceDrafts[editingDevice.id].tgRewrites" :key="`tg-${index}`" class="rewrite-row">
                    <input v-model="rule.fromSlot" :placeholder="t('device.fromSlot')" />
                    <input v-model="rule.fromTG" :placeholder="t('device.fromTG')" />
                    <input v-model="rule.toSlot" :placeholder="t('device.toSlot')" />
                    <input v-model="rule.toTG" :placeholder="t('device.toTG')" />
                    <input v-model="rule.range" :placeholder="t('device.range')" />
                    <button type="button" class="ghost danger" @click="removeRewriteRule(editingDevice.id, 'tgRewrites', index)">{{ t('app.delete') }}</button>
                  </div>
                </div>
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.pcRewrite') }}</strong>
                    <button type="button" class="ghost" @click="addRewriteRule(editingDevice.id, 'pcRewrites', 'pc')">{{ t('device.addRule') }}</button>
                  </div>
                  <div v-if="!deviceDrafts[editingDevice.id].pcRewrites.length" class="hint">{{ t('device.noRules') }}</div>
                  <div v-for="(rule, index) in deviceDrafts[editingDevice.id].pcRewrites" :key="`pc-${index}`" class="rewrite-row">
                    <input v-model="rule.fromSlot" :placeholder="t('device.fromSlot')" />
                    <input v-model="rule.fromId" :placeholder="t('device.fromId')" />
                    <input v-model="rule.toSlot" :placeholder="t('device.toSlot')" />
                    <input v-model="rule.toId" :placeholder="t('device.toId')" />
                    <input v-model="rule.range" :placeholder="t('device.range')" />
                    <button type="button" class="ghost danger" @click="removeRewriteRule(editingDevice.id, 'pcRewrites', index)">{{ t('app.delete') }}</button>
                  </div>
                </div>
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.typeRewrite') }}</strong>
                    <button type="button" class="ghost" @click="addRewriteRule(editingDevice.id, 'typeRewrites', 'type')">{{ t('device.addRule') }}</button>
                  </div>
                  <div v-if="!deviceDrafts[editingDevice.id].typeRewrites.length" class="hint">{{ t('device.noRules') }}</div>
                  <div v-for="(rule, index) in deviceDrafts[editingDevice.id].typeRewrites" :key="`type-${index}`" class="rewrite-row">
                    <input v-model="rule.fromSlot" :placeholder="t('device.fromSlot')" />
                    <input v-model="rule.fromTG" :placeholder="t('device.fromTG')" />
                    <input v-model="rule.toSlot" :placeholder="t('device.toSlot')" />
                    <input v-model="rule.toId" :placeholder="t('device.toId')" />
                    <input v-model="rule.range" :placeholder="t('device.range')" />
                    <button type="button" class="ghost danger" @click="removeRewriteRule(editingDevice.id, 'typeRewrites', index)">{{ t('app.delete') }}</button>
                  </div>
                </div>
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.srcRewrite') }}</strong>
                    <button type="button" class="ghost" @click="addRewriteRule(editingDevice.id, 'srcRewrites', 'src')">{{ t('device.addRule') }}</button>
                  </div>
                  <div v-if="!deviceDrafts[editingDevice.id].srcRewrites.length" class="hint">{{ t('device.noRules') }}</div>
                  <div v-for="(rule, index) in deviceDrafts[editingDevice.id].srcRewrites" :key="`src-${index}`" class="rewrite-row">
                    <input v-model="rule.fromSlot" :placeholder="t('device.fromSlot')" />
                    <input v-model="rule.fromId" :placeholder="t('device.fromId')" />
                    <input v-model="rule.toSlot" :placeholder="t('device.toSlot')" />
                    <input v-model="rule.toId" :placeholder="t('device.toId')" />
                    <input v-model="rule.range" :placeholder="t('device.range')" />
                    <button type="button" class="ghost danger" @click="removeRewriteRule(editingDevice.id, 'srcRewrites', index)">{{ t('app.delete') }}</button>
                  </div>
                </div>
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.passAllTG') }}</strong>
                  </div>
                  <div class="slot-radio-options">
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].passAllTG" type="checkbox" :value="1" />
                      <span>TS1</span>
                    </label>
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].passAllTG" type="checkbox" :value="2" />
                      <span>TS2</span>
                    </label>
                  </div>
                </div>
                <div class="rewrite-group">
                  <div class="rewrite-group-head">
                    <strong>{{ t('device.passAllPC') }}</strong>
                  </div>
                  <div class="slot-radio-options">
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].passAllPC" type="checkbox" :value="1" />
                      <span>TS1</span>
                    </label>
                    <label class="slot-radio-option">
                      <input v-model="deviceDrafts[editingDevice.id].passAllPC" type="checkbox" :value="2" />
                      <span>TS2</span>
                    </label>
                  </div>
                </div>
              </div>
            </section>

            <section class="device-editor-section field-span-2">
              <div class="device-editor-section-head">
                <h3>{{ t('device.notes') }}</h3>
              </div>
              <div class="device-editor-section-grid">
                <label class="field-block field-span-2">
                  <span>{{ t('device.notes') }}</span>
                  <textarea v-model="deviceDrafts[editingDevice.id].notes" rows="3" :placeholder="t('device.notes')"></textarea>
                </label>
              </div>
            </section>
          </div>
          <div class="device-editor-actions">
            <button type="button" class="ghost" @click="closeDeviceEditor()">{{ t('app.close') }}</button>
            <button type="submit" class="primary">{{ t('app.save') }}</button>
          </div>
        </form>
      </section>
    </div>

    <div v-if="accountCreatorOpen" class="modal-backdrop">
      <section class="auth-modal glass">
        <div class="section-head">
          <h2>{{ t('app.create') }} {{ t('app.accounts') }}</h2>
          <button class="ghost" @click="accountCreatorOpen = false">{{ t('app.close') }}</button>
        </div>
        <form class="form-grid account-create-panel" @submit.prevent="createUser().then(() => { accountCreatorOpen = false })">
          <div class="account-create-grid">
            <label class="field-block">
              <span>{{ t('auth.usernameOnly') }}</span>
              <input v-model="userForm.username" :placeholder="t('auth.usernameOnly')" />
            </label>
            <label class="field-block">
              <span>{{ t('auth.callsign') }}</span>
              <input v-model="userForm.callsign" :placeholder="t('auth.callsign')" @input="syncUserCallsign" />
            </label>
            <label class="field-block field-span-2">
              <span>{{ t('auth.email') }}</span>
              <input v-model="userForm.email" :placeholder="t('auth.email')" />
            </label>
            <label class="field-block">
              <span>{{ t('auth.password') }}</span>
              <input v-model="userForm.password" type="password" :placeholder="t('auth.password')" />
            </label>
            <label class="field-block">
              <span>{{ t('auth.role') }}</span>
              <select v-model="userForm.role">
                <option value="ham">{{ t('auth.ham') }}</option>
                <option value="admin">{{ t('auth.admin') }}</option>
              </select>
            </label>
          </div>
          <div class="account-create-footer">
            <label class="checkbox-line"><input v-model="userForm.enabled" type="checkbox" /> {{ t('app.enabled') }}</label>
            <button class="primary">{{ t('app.create') }}</button>
          </div>
        </form>
      </section>
    </div>
  </div>
</template>
