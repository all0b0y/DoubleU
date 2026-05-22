import { api, openEvents } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function initials(name) {
  if (!name) return '?';
  return name.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase();
}

function formatTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  return d.toLocaleTimeString('ru', { hour: '2-digit', minute: '2-digit' });
}

export function renderChat(navigate, chatId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="topbar">
      <button class="btn-icon" id="btn-back">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M15 18l-6-6 6-6"/></svg>
      </button>
      <div class="avatar avatar-sm" id="chat-avatar" style="flex-shrink:0"></div>
      <div style="flex:1;min-width:0">
        <div class="topbar-title" id="chat-name" style="font-size:15px">…</div>
        <div style="font-size:11px;color:var(--hint)" id="chat-mode-label"></div>
      </div>
      <button class="topbar-action" id="btn-settings">⚙️</button>
    </div>
    <div class="scroll" id="msg-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
    <div id="drafts-area"></div>
  `;

  el.querySelector('#btn-back').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/chats');
  });

  el.querySelector('#btn-settings').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate(`/chat/${chatId}/settings`);
  });

  // Wire Telegram BackButton
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(() => navigate('/chats'));

  let chatName = chatId;
  let chatMode = '';
  let currentDrafts = [];

  function renderMessages(messages) {
    const list = el.querySelector('#msg-list');
    if (!messages.length) {
      list.innerHTML = `<div class="empty-state"><div class="empty-icon">💬</div><div class="empty-subtitle">Нет сообщений</div></div>`;
      return;
    }
    list.innerHTML = `<div class="messages-list">${
      messages.map(m => {
        const isOwner = m.type === 'outgoing' || m.sender_id === 'owner';
        return `
          <div class="msg-bubble ${isOwner ? 'outgoing' : 'incoming'}">
            ${m.text || '—'}
            <div class="msg-time">${formatTime(m.created_at)}</div>
          </div>
        `;
      }).join('')
    }</div>`;
    list.scrollTop = list.scrollHeight;
  }

  function renderDrafts(drafts) {
    const area = el.querySelector('#drafts-area');
    const pending = drafts.filter(d => d.status === 'pending');
    currentDrafts = pending;

    if (!pending.length) {
      area.innerHTML = '';
      twa?.MainButton?.hide();
      return;
    }

    area.innerHTML = pending.map(d => `
      <div class="draft-card" data-id="${d.draft_id}">
        <div class="draft-card-header">
          <span>✨</span>
          <div class="draft-card-header-title">Черновик DoubleU</div>
        </div>
        ${d.source_text ? `<div class="draft-source">«${d.source_text}»</div>` : ''}
        <div class="draft-text">${d.response_text}</div>
        <div class="draft-actions">
          <button class="draft-btn danger" data-action="reject" data-id="${d.draft_id}">✗ Отклонить</button>
          <button class="draft-btn" data-action="regenerate" data-id="${d.draft_id}">🔄 Ещё раз</button>
          <button class="draft-btn primary" data-action="approve" data-id="${d.draft_id}">✓ Отправить</button>
        </div>
      </div>
    `).join('');

    // Wire action buttons
    area.querySelectorAll('[data-action]').forEach(btn => {
      btn.addEventListener('click', () => handleDraftAction(btn.dataset.id, btn.dataset.action));
    });

    // Show MainButton for the first pending draft approval
    if (pending.length > 0) {
      const first = pending[0];
      twa?.MainButton?.setText('Отправить черновик');
      twa?.MainButton?.setParams({ color: '#6366F1', text_color: '#FFFFFF' });
      twa?.MainButton?.show();
      twa?.MainButton?.onClick(() => handleDraftAction(first.draft_id, 'approve'));
    }
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
        if (btnId !== 'reject') return;
        await executeDraftAction(draftId, 'reject');
      });
      return;
    }

    await executeDraftAction(draftId, action);
  }

  async function executeDraftAction(draftId, action) {
    const btn = el.querySelector(`[data-action="${action}"][data-id="${draftId}"]`);
    if (btn) { btn.disabled = true; btn.style.opacity = '0.5'; }

    try {
      await api.draftAction(chatId, draftId, action);
      twa?.HapticFeedback?.notificationOccurred('success');
      await loadDrafts();
      if (action === 'approve') await loadMessages();
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      if (btn) { btn.disabled = false; btn.style.opacity = ''; }
    }
  }

  async function loadMessages() {
    try {
      const data = await api.messages(chatId);
      renderMessages(data.messages || []);
    } catch (_) {}
  }

  async function loadDrafts() {
    try {
      const data = await api.drafts(chatId);
      renderDrafts(data.drafts || []);
    } catch (_) {}
  }

  async function loadChatMeta() {
    try {
      const [chatsData, settingsData] = await Promise.all([api.chats(), api.chatSettings(chatId)]);
      const chat = chatsData.chats?.find(c => c.chat_id === chatId);
      if (chat) {
        chatName = chat.display_name || chatId;
        chatMode = chat.mode;
        el.querySelector('#chat-name').textContent = chatName;
        el.querySelector('#chat-avatar').textContent = chat.display_name?.split(/\s+/).map(w => w[0]).join('').slice(0, 2).toUpperCase() || '?';
      }
      if (settingsData) {
        el.querySelector('#chat-mode-label').textContent = modeLabel(settingsData.mode);
      }
    } catch (_) {}
  }

  function modeLabel(mode) {
    return { auto: 'Полная автономия', semi_auto: 'Полуавтомат', manual: 'Ручной режим', silent: 'Наблюдение', disabled: 'Отключено' }[mode] || mode;
  }

  // Real-time events for this chat
  const es = openEvents({
    new_draft: (d) => { if (d.chat_id === chatId) loadDrafts(); },
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
      twa?.BackButton?.hide();
      twa?.MainButton?.hide();
      es.close();
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  // Initial load
  loadChatMeta();
  loadMessages();
  loadDrafts();

  return el;
}
