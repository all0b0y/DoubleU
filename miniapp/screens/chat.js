import { api, openEvents } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function initials(name) {
  if (!name) return '?';
  return name.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase();
}

function formatTime(iso) {
  if (!iso) return '';
  return new Date(iso).toLocaleTimeString('ru', { hour: '2-digit', minute: '2-digit' });
}

function dateSepLabel(iso) {
  const d = new Date(iso);
  const today = new Date();
  const yest  = new Date(today); yest.setDate(today.getDate() - 1);
  if (d.toDateString() === today.toDateString()) return 'сегодня';
  if (d.toDateString() === yest.toDateString())  return 'вчера';
  return d.toLocaleDateString('ru', { day: 'numeric', month: 'long' });
}

const MODE_LABELS = {
  auto: '⚡ auto', semi_auto: '⚡ semi_auto', manual: '✋ manual',
  silent: 'silent', disabled: 'disabled',
};
const MODE_COLORS = {
  auto: 'var(--success)', semi_auto: 'var(--accent)',
  manual: 'var(--warning)', silent: 'var(--muted)', disabled: 'var(--muted)',
};

export function renderChat(navigate, chatId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-av" id="chat-avatar"></div>
      <div class="chat-topbar-info">
        <div class="chat-topbar-name" id="chat-name">…</div>
        <div class="chat-topbar-mode" id="chat-mode"></div>
      </div>
      <button class="chat-topbar-btn" id="btn-profile" title="Досье">ⓘ</button>
      <button class="chat-topbar-btn" id="btn-settings" title="Настройки">⚙</button>
    </div>

    <div class="scroll" id="msg-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>

    <div id="draft-panel"></div>
  `;

  const handleBack = () => navigate('/chats');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);

  el.querySelector('#btn-settings').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate(`/chat/${chatId}/settings`);
  });
  el.querySelector('#btn-profile').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate(`/chat/${chatId}/profile`);
  });

  let currentDrafts = [];

  // ── Messages ──────────────────────────────────────────────────────────────

  function renderMessages(messages) {
    const list = el.querySelector('#msg-list');
    if (!messages.length) {
      list.innerHTML = `<div class="empty-state"><div class="empty-icon">💬</div><div class="empty-subtitle">Нет сообщений</div></div>`;
      return;
    }

    let html = '<div class="msg-list-inner">';
    let lastDateStr = null;

    for (const m of messages) {
      const dateStr = dateSepLabel(m.created_at);
      if (dateStr !== lastDateStr) {
        html += `<div class="msg-date-sep"><span>${dateStr}</span></div>`;
        lastDateStr = dateStr;
      }
      const out = m.direction === 'outgoing';
      if (out) {
        const footer = m.label
          ? `<div class="msg-footer-out">
               <span class="msg-footer-du">✨ ${m.label}</span>
               <span class="msg-footer-time">${formatTime(m.created_at)} ✓✓</span>
             </div>`
          : `<p class="msg-time-in" style="color:rgba(255,255,255,0.5)">${formatTime(m.created_at)}</p>`;
        html += `
          <div class="msg-bubble msg-out">
            <p class="msg-text">${m.text || '—'}</p>
            ${footer}
          </div>`;
      } else {
        html += `
          <div class="msg-bubble msg-in">
            <p class="msg-text">${m.text || '—'}</p>
            <p class="msg-time-in">${formatTime(m.created_at)}</p>
          </div>`;
      }
    }
    html += '</div>';
    list.innerHTML = html;
    list.scrollTop = list.scrollHeight;
  }

  // ── Draft panel ───────────────────────────────────────────────────────────

  function renderDraftPanel(drafts) {
    const panel = el.querySelector('#draft-panel');
    const pending = drafts.filter(d => d.status === 'pending');
    currentDrafts = pending;
    twa?.MainButton?.hide();

    if (!pending.length) {
      panel.innerHTML = '';
      return;
    }

    const d = pending[0];
    panel.innerHTML = `
      <div class="draft-panel">
        <div class="draft-panel-header">
          <div class="draft-panel-label">
            <div class="draft-panel-dot">✨</div>
            <span class="draft-panel-title">черновик</span>
          </div>
          <div style="display:flex;gap:2px;">
            <button class="draft-icon-btn" data-action="thumb_up"   data-id="${d.draft_id}" title="Нравится">👍</button>
            <button class="draft-icon-btn" data-action="thumb_down" data-id="${d.draft_id}" title="Не нравится">👎</button>
          </div>
        </div>

        <div class="draft-bubble">
          <p class="draft-bubble-text">${d.response_text}</p>
          <div class="draft-bubble-actions">
            <button class="draft-text-btn" data-action="edit_request" data-id="${d.draft_id}">✏ править</button>
            <button class="draft-text-btn" data-action="ai_refine"   data-id="${d.draft_id}">💬 уточнить</button>
            <button class="draft-text-btn" data-action="regenerate"  data-id="${d.draft_id}">🔄 ещё</button>
          </div>
        </div>

        <div class="draft-send-row">
          <button class="draft-trash-btn" data-action="reject"  data-id="${d.draft_id}" title="Отклонить">🗑</button>
          <button class="draft-send-btn"  data-action="approve" data-id="${d.draft_id}">↑ отправить</button>
        </div>
      </div>
    `;

    panel.querySelectorAll('[data-action]').forEach(btn => {
      btn.addEventListener('click', () => handleDraftAction(btn.dataset.id, btn.dataset.action));
    });
  }

  async function handleDraftAction(draftId, action) {
    twa?.HapticFeedback?.impactOccurred('medium');
    if (action === 'reject') {
      twa?.showPopup({
        title: 'Отклонить черновик?',
        message: 'Сообщение не будет отправлено.',
        buttons: [
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
          { id: 'reject', type: 'destructive', text: 'Отклонить' },
        ],
      }, async (btnId) => {
        if (btnId === 'reject') await executeDraftAction(draftId, 'reject');
      });
      return;
    }
    await executeDraftAction(draftId, action);
  }

  async function executeDraftAction(draftId, action) {
    el.querySelectorAll(`[data-id="${draftId}"]`).forEach(b => { b.disabled = true; b.style.opacity = '0.5'; });
    try {
      await api.draftAction(chatId, draftId, action);
      twa?.HapticFeedback?.notificationOccurred('success');
      await loadDrafts();
      if (action === 'approve') await loadMessages();
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      el.querySelectorAll(`[data-id="${draftId}"]`).forEach(b => { b.disabled = false; b.style.opacity = ''; });
    }
  }

  // ── Data loaders ──────────────────────────────────────────────────────────

  async function loadMessages() {
    try {
      const data = await api.messages(chatId);
      renderMessages(data.messages || []);
    } catch (_) {}
  }

  async function loadDrafts() {
    try {
      const data = await api.drafts(chatId);
      renderDraftPanel(data.drafts || []);
    } catch (_) {}
  }

  async function loadChatMeta() {
    try {
      const [chatsData, settingsData] = await Promise.all([api.chats(), api.chatSettings(chatId)]);
      const chat = chatsData.chats?.find(c => c.chat_id === chatId);
      if (chat) {
        el.querySelector('#chat-name').textContent = chat.display_name || chatId;
        el.querySelector('#chat-avatar').textContent = initials(chat.display_name);
      }
      if (settingsData) {
        const modeEl = el.querySelector('#chat-mode');
        modeEl.textContent = MODE_LABELS[settingsData.mode] || settingsData.mode;
        modeEl.style.color  = MODE_COLORS[settingsData.mode] || 'var(--hint)';
      }
    } catch (_) {}
  }

  // ── Real-time ─────────────────────────────────────────────────────────────

  const es = openEvents({
    new_draft:    (d) => { if (d.chat_id === chatId) loadDrafts(); },
    draft_status: (d) => {
      if (currentDrafts.find(x => x.draft_id === d.draft_id)) {
        loadDrafts();
        if (d.status === 'sent') loadMessages();
      }
    },
    new_message: (d) => { if (d.chat_id === chatId) loadMessages(); },
  });

  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      twa?.MainButton?.hide();
      es.close();
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  loadChatMeta();
  loadMessages();
  loadDrafts();

  return el;
}
