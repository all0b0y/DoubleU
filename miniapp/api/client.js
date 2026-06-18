// API client — wraps all backend calls with TMA auth header.

const twa = window.Telegram?.WebApp;

/** Raw TMA initData string used for auth. In development, falls back to a dummy value. */
export function getInitData() {
  if (twa?.initData) return twa.initData;
  // Dev fallback: pass user_id directly without validation (server skips validation when no BOT_TOKEN)
  const devUser = localStorage.getItem('dev_user_id') || '123456789';
  return `user_id=${devUser}&auth_date=${Math.floor(Date.now() / 1000)}`;
}

async function request(method, path, body) {
  const headers = { 'Authorization': `tma ${getInitData()}` };
  const opts = { method, headers };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(err.error || res.statusText);
  }
  return res.json();
}

export const api = {
  me:           ()              => request('GET',   '/api/v1/me'),
  chats:        ()              => request('GET',   '/api/v1/chats'),
  addChats:     (chatIds)       => request('POST',  '/api/v1/chats', { chat_ids: chatIds }),
  messages:     (chatId)        => request('GET',   `/api/v1/chats/${chatId}/messages`),
  sendMessage:  (chatId, text)  => request('POST',  `/api/v1/chats/${chatId}/send`, { text }),
  drafts:       (chatId)        => request('GET',   `/api/v1/chats/${chatId}/drafts`),
  draftAction:  (chatId, draftId, action, editedText) =>
    request('POST', `/api/v1/chats/${chatId}/drafts/${draftId}/action`,
      { action, ...(editedText ? { edited_text: editedText } : {}) }),
  chatSettings: (chatId)        => request('GET',   `/api/v1/chats/${chatId}/settings`),
  updateSettings: (chatId, patch) => request('PATCH', `/api/v1/chats/${chatId}/settings`, patch),
  contacts:       ()              => request('GET',   '/api/v1/contacts'),
  getProfile:     (chatId)        => request('GET',   `/api/v1/chats/${chatId}/profile`),
  saveProfile:    (chatId, data)  => request('PUT',   `/api/v1/chats/${chatId}/profile`, data),
  avatarURL:      (chatId)        => `/api/v1/chats/${chatId}/avatar`,

  // AI roles (personas)
  roles:          ()              => request('GET',    '/api/v1/roles'),
  createRole:     (data)          => request('POST',   '/api/v1/roles', data),
  updateRole:     (roleId, data)  => request('PUT',    `/api/v1/roles/${roleId}`, data),
  deleteRole:     (roleId)        => request('DELETE', `/api/v1/roles/${roleId}`),
  assignRole:     (chatId, roleId) => request('PUT',   `/api/v1/chats/${chatId}/role`, { role_id: roleId }),

  // AI profile suggestions
  suggestions:    (chatId)        => request('GET',
    chatId ? `/api/v1/chats/${chatId}/suggestions` : '/api/v1/suggestions'),
  suggestionAction: (id, action)  => request('POST',  `/api/v1/suggestions/${id}/action`, { action }),

  // Memory: summary, facts, episodes, erasure
  summarize:      (chatId)        => request('POST',   `/api/v1/chats/${chatId}/summary`),
  facts:          (chatId)        => request('GET',    `/api/v1/chats/${chatId}/facts`),
  deleteFact:     (factId)        => request('DELETE', `/api/v1/facts/${factId}`),
  episodes:       (chatId)        => request('GET',    `/api/v1/chats/${chatId}/episodes`),
  forgetChat:     (chatId)        => request('POST',   `/api/v1/chats/${chatId}/forget`),
  forgetAll:      (phrase)        => request('POST',   '/api/v1/forget-all', { confirm_phrase: phrase }),

  // Owner-level settings ("Я" screen)
  userSettings:     ()            => request('GET',   '/api/v1/settings/user'),
  saveUserSettings: (data)        => request('PUT',   '/api/v1/settings/user', data),

  // Bot ingestion (user-supplied bots — a tenant may connect several)
  bots:          ()              => request('GET',    '/api/v1/bots'),
  registerBot:   (token)         => request('POST',   '/api/v1/bots', { token }),
  setBotRole:    (botId, roleId) => request('PUT',    `/api/v1/bots/${botId}/role`, { role_id: roleId }),
  updateBotSettings: (botId, data) => request('PUT',  `/api/v1/bots/${botId}/role`, data),
  disconnectBot: (botId)         => request('DELETE', `/api/v1/bots/${botId}`),
};

/** Open an SSE stream; returns the EventSource. Caller owns closing it. */
export function openEvents(handlers) {
  const initData = encodeURIComponent(getInitData());
  const es = new EventSource(`/api/v1/events?auth=${initData}`);
  for (const [type, fn] of Object.entries(handlers)) {
    es.addEventListener(type, (e) => {
      try { fn(JSON.parse(e.data)); } catch (_) {}
    });
  }
  es.onerror = () => {
    // Auto-reconnect is handled by EventSource; no manual retry needed.
  };
  return es;
}
