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
