import { api, openEvents } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const MODE_LABELS = {
  auto:      'auto',
  semi_auto: 'semi',
  manual:    'manual',
  silent:    'silent',
  disabled:  'off',
};

function initials(name) {
  if (!name) return '?';
  return name.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase();
}

function timeAgo(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  const diff = (Date.now() - d) / 1000;
  if (diff < 60)    return 'сейчас';
  if (diff < 3600)  return `${Math.floor(diff/60)} мин`;
  if (diff < 86400) return `${Math.floor(diff/3600)} ч`;
  return d.toLocaleDateString('ru', { day: 'numeric', month: 'short' });
}

export function renderChats(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="topbar">
      <div class="topbar-title">Чаты</div>
      <button class="topbar-action" id="btn-add" title="Добавить чат">＋</button>
    </div>
    <div class="search-bar">
      <span class="search-icon">🔍</span>
      <input type="search" placeholder="Поиск" id="search-input">
    </div>
    <div class="scroll" id="chat-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  let allChats = [];

  function renderList(filter = '') {
    const list = el.querySelector('#chat-list');
    const filtered = filter
      ? allChats.filter(c => c.display_name.toLowerCase().includes(filter.toLowerCase()))
      : allChats;

    if (!filtered.length) {
      list.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">💬</div>
          <div class="empty-title">Нет чатов</div>
          <div class="empty-subtitle">Добавь чаты через кнопку ＋ выше</div>
        </div>`;
      return;
    }

    list.innerHTML = filtered.map(chat => `
      <div class="chat-item" data-id="${chat.chat_id}">
        <div class="avatar avatar-md">${initials(chat.display_name)}</div>
        <div class="chat-item-body">
          <div class="chat-item-row1">
            <div class="chat-item-name">${chat.display_name || chat.chat_id}</div>
            <div class="chat-item-time">${timeAgo(chat.last_seen_at)}</div>
          </div>
          <div class="chat-item-row2">
            <div class="chat-item-preview">${chat.last_message || 'Нет сообщений'}</div>
            <div style="display:flex;gap:5px;align-items:center">
              ${chat.draft_count > 0 ? `<div class="draft-badge">${chat.draft_count}</div>` : ''}
              <div class="mode-badge ${chat.mode}">${MODE_LABELS[chat.mode] || chat.mode}</div>
            </div>
          </div>
        </div>
      </div>
    `).join('');

    list.querySelectorAll('.chat-item').forEach(item => {
      item.addEventListener('click', () => {
        twa?.HapticFeedback?.selectionChanged();
        navigate(`/chat/${item.dataset.id}`);
      });
    });
  }

  async function loadChats() {
    try {
      const data = await api.chats();
      allChats = data.chats || [];
      renderList(el.querySelector('#search-input').value);
    } catch (err) {
      el.querySelector('#chat-list').innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">⚠️</div>
          <div class="empty-title">Ошибка загрузки</div>
          <div class="empty-subtitle">${err.message}</div>
        </div>`;
    }
  }

  el.querySelector('#btn-add').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/add-chat');
  });

  el.querySelector('#search-input').addEventListener('input', (e) => {
    renderList(e.target.value);
  });

  // Real-time: refresh on new drafts and mode changes
  const es = openEvents({
    new_draft: () => loadChats(),
    chat_mode_changed: () => loadChats(),
  });

  // Clean up SSE when navigated away
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { es.close(); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  loadChats();
  return el;
}
