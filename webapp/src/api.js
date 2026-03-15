async function request(path, options = {}) {
  const response = await fetch(path, {
    credentials: 'include',
    headers: {
      'Content-Type': 'application/json',
      ...(options.headers || {})
    },
    ...options
  })
  if (!response.ok) {
    const text = await response.text()
    throw new Error(text || response.statusText)
  }
  if (response.status === 204) {
    return null
  }
  return response.json()
}

export const api = {
  me: () => request('/api/auth/me'),
  login: (payload) => request('/api/auth/login', { method: 'POST', body: JSON.stringify(payload) }),
  logout: () => request('/api/auth/logout', { method: 'POST' }),
  register: (payload) => request('/api/auth/register', { method: 'POST', body: JSON.stringify(payload) }),
  snapshot: () => request('/api/snapshot'),
  listUsers: () => request('/api/users'),
  createUser: (payload) => request('/api/users', { method: 'POST', body: JSON.stringify(payload) }),
  createDevice: (payload) => request('/api/devices', { method: 'POST', body: JSON.stringify(payload) }),
  updateUser: (id, payload) => request(`/api/users/${id}`, { method: 'PATCH', body: JSON.stringify(payload) }),
  deleteUser: (id) => request(`/api/users/${id}`, { method: 'DELETE' }),
  resetUserPassword: (id, password) => request(`/api/users/${id}/reset-password`, { method: 'POST', body: JSON.stringify({ password }) }),
  updateDevice: (id, payload) => request(`/api/devices/${id}`, { method: 'PATCH', body: JSON.stringify(payload) }),
  deleteDevice: (id) => request(`/api/devices/${id}`, { method: 'DELETE' })
}
