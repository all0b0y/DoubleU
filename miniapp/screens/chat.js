import { api, openEvents } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

// Labels for non-text message types (issue 1)
const MEDIA_LABEL = {
  photo:      '📷 Фото',
  video:      '🎥 Видео',
  voice:      '🎤 Голосовое',
  document:   '📎 Файл',
  sticker:    '🎭 Стикер',
  audio:      '🎵 Аудио',
  video_note: '📹 Кружок',
  animation:  '🎬 GIF',
  caption:    '🖼 Медиа',
  unknown:    '📎 Медиа',
};

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

function msgText(m) {
  if (m.text) return { html: esc(m.text), isMedia: false };
  const label = MEDIA_LABEL[m.message_type] || MEDIA_LABEL.unknown;
  return { html: `<span style="color:var(--hint);font-style:italic">${label}</span>`, isMedia: true };
}

function esc(str) {
  return String(str || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
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

  const THINKING_PHRASES = [
    '✨ ИИ подбирает слова…',
    '🤔 ИИ думает как лучше ответить…',
    '💭 ИИ перебирает варианты…',
    '🧠 ИИ советуется с музами…',
    '⚡ ИИ строит стратегию…',
    '🎭 ИИ репетирует ответ…',
    '🪄 ИИ колдует над ответом…',
  ];

  function showThinkingIndicator() {
    const list = el.querySelector('#msg-list');
    if (list.querySelector('#ai-thinking-indicator')) return;
    const phrase = THINKING_PHRASES[Math.floor(Math.random() * THINKING_PHRASES.length)];
    const div = document.createElement('div');
    div.id = 'ai-thinking-indicator';
    div.className = 'msg-bubble msg-out';
    div.style.cssText = 'opacity:0.65;font-style:italic;';
    div.innerHTML = `<p class="msg-text" style="color:var(--hint)">${phrase}</p>`;
    const inner = list.querySelector('.msg-list-inner');
    (inner || list).appendChild(div);
    list.scrollTop = list.scrollHeight;
  }

  function hideThinkingIndicator() {
    el.querySelector('#ai-thinking-indicator')?.remove();
  }

  // ── Avatar (issue 3) ──────────────────────────────────────────────────────

  function loadAvatar() {
    const avatarEl = el.querySelector('#chat-avatar');
    const url = api.avatarURL(chatId);
    const img = new Image();
    img.onload = () => {
      avatarEl.innerHTML = '';
      img.style.cssText = 'width:100%;height:100%;object-fit:cover;border-radius:50%;';
      avatarEl.appendChild(img);
    };
    img.src = url;
  }

  // ── Messages ──────────────────────────────────────────────────────────────

  function renderDraftBubble(d) {
    const sourceHtml = d.source_text
      ? `<div class="draft-source-msg">
           <span class="draft-source-label">↩ на сообщение:</span>
           <span class="draft-source-text">${esc(d.source_text)}</span>
         </div>`
      : '';
    return `
      <div class="msg-bubble msg-out msg-draft" data-draft-id="${d.draft_id}">
        ${sourceHtml}
        <div class="draft-bubble">
          <p class="draft-bubble-text">${esc(d.response_text)}</p>
          <div class="draft-bubble-actions">
            <button class="draft-text-btn" data-action="edit_enter"   data-id="${d.draft_id}">✏ править</button>
            <button class="draft-text-btn" data-action="refine_enter" data-id="${d.draft_id}">💬 уточнить</button>
            <button class="draft-text-btn" data-action="regenerate"   data-id="${d.draft_id}">🔄 ещё</button>
          </div>
        </div>
        <div class="draft-send-row" style="margin-top:6px">
          <button class="draft-trash-btn" data-action="reject"  data-id="${d.draft_id}" title="Отклонить">🗑</button>
          <button class="draft-send-btn"  data-action="approve" data-id="${d.draft_id}">↑ отправить</button>
        </div>
        <div class="msg-footer-out" style="margin-top:4px">
          <span class="msg-footer-du">✨ черновик</span>
          <span class="msg-footer-time">${formatTime(d.created_at)}</span>
        </div>
      </div>`;
  }

  function renderMessages(messages, drafts = []) {
    const list = el.querySelector('#msg-list');

    const pending = drafts.filter(d => d.status === 'pending');
    currentDrafts = pending;

    // Merge pending drafts into message timeline
    const draftItems = pending.map(d => ({ ...d, is_draft: true, direction: 'outgoing' }));
    const allItems = [...messages, ...draftItems].sort((a, b) =>
      new Date(a.created_at) - new Date(b.created_at)
    );

    if (!allItems.length) {
      list.innerHTML = `<div class="empty-state"><div class="empty-icon">💬</div><div class="empty-subtitle">Нет сообщений</div></div>`;
      return;
    }

    let html = '<div class="msg-list-inner">';
    let lastDateStr = null;

    for (const m of allItems) {
      const dateStr = dateSepLabel(m.created_at);
      if (dateStr !== lastDateStr) {
        html += `<div class="msg-date-sep"><span>${dateStr}</span></div>`;
        lastDateStr = dateStr;
      }

      if (m.is_draft) {
        html += renderDraftBubble(m);
        continue;
      }

      const out = m.direction === 'outgoing';
      const { html: textHtml } = msgText(m);
      if (out) {
        const footer = m.label
          ? `<div class="msg-footer-out">
               <span class="msg-footer-du">✨ ${m.label}</span>
               <span class="msg-footer-time">${formatTime(m.created_at)} ✓✓</span>
             </div>`
          : `<div class="msg-footer-out">
               <span class="msg-footer-du" style="opacity:0.5">Ты</span>
               <span class="msg-footer-time">${formatTime(m.created_at)}</span>
             </div>`;
        html += `
          <div class="msg-bubble msg-out">
            <p class="msg-text">${textHtml}</p>
            ${footer}
          </div>`;
      } else {
        html += `
          <div class="msg-bubble msg-in">
            <p class="msg-text">${textHtml}</p>
            <p class="msg-time-in">${formatTime(m.created_at)}</p>
          </div>`;
      }
    }
    html += '</div>';
    list.innerHTML = html;

    // Scroll to first pending draft so user sees it immediately; otherwise to bottom
    const firstDraft = list.querySelector('.msg-draft');
    if (firstDraft) {
      firstDraft.scrollIntoView({ behavior: 'instant', block: 'center' });
    } else {
      list.scrollTop = list.scrollHeight;
    }

    // Attach action listeners to inline draft bubbles
    list.querySelectorAll('.msg-draft [data-action]').forEach(btn => {
      btn.addEventListener('click', () => {
        const action = btn.dataset.action;
        const id = btn.dataset.id;
        const draft = pending.find(d => d.draft_id === id);
        const panel = el.querySelector('#draft-panel');
        twa?.HapticFeedback?.impactOccurred('light');
        if (action === 'edit_enter' && draft) {
          renderDraftView(panel, draft, 'editing');
          panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
        } else if (action === 'refine_enter' && draft) {
          renderDraftView(panel, draft, 'refining');
          panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
        } else {
          handleDraftAction(id, action);
        }
      });
    });
  }

  // ── Draft panel ───────────────────────────────────────────────────────────

  // Draft panel is now used only for editing/refining modes (normal view is inline in the message list).
  function renderDraftPanel(drafts) {
    const panel = el.querySelector('#draft-panel');
    twa?.MainButton?.hide();
    // Clear stale editing UI when there are no more pending drafts.
    const hasPending = drafts.some(d => d.status === 'pending');
    if (!hasPending) panel.innerHTML = '';
  }

  // mode: 'normal' | 'editing' | 'refining'
  //
  // normal   — read-only draft text, actions: ✏ | 💬 | 🔄,  send row: 🗑 | ↑
  // editing  — editable textarea,    actions: 💬 | 🔄,       send row: ← | 💾
  // refining — hint textarea,                                 send row: ← | ✨
  //
  // 💬 уточнить is present in BOTH normal and editing modes so it never disappears.
  function renderDraftView(panel, d, mode) {
    const sourceHtml = d.source_text
      ? `<div class="draft-source-msg">
           <span class="draft-source-label">↩ на сообщение:</span>
           <span class="draft-source-text">${esc(d.source_text)}</span>
         </div>`
      : '';

    let bubbleInner, sendRow, title;

    if (mode === 'editing') {
      title = 'редактирование';
      bubbleInner = `
        <textarea class="draft-edit-textarea" id="draft-edit-input">${esc(d.response_text)}</textarea>
        <div class="draft-bubble-actions">
          <button class="draft-text-btn" data-action="refine_enter" data-id="${d.draft_id}">💬 уточнить</button>
          <button class="draft-text-btn" data-action="regenerate"   data-id="${d.draft_id}">🔄 ещё</button>
        </div>`;
      sendRow = `
        <button class="draft-back-btn"  data-action="mode_normal" data-id="${d.draft_id}">← назад</button>
        <button class="draft-send-btn"  data-action="save_edit"   data-id="${d.draft_id}">💾 сохранить</button>`;

    } else if (mode === 'refining') {
      title = 'уточнить ИИ';
      bubbleInner = `
        <p class="draft-refine-label">Что изменить?</p>
        <textarea class="draft-edit-textarea" id="draft-refine-input"
          placeholder="Напиши, что изменить…" style="min-height:52px;"></textarea>`;
      sendRow = `
        <button class="draft-back-btn"  data-action="mode_normal" data-id="${d.draft_id}">← назад</button>
        <button class="draft-send-btn"  data-action="do_refine"   data-id="${d.draft_id}">✨ уточнить</button>`;

    } else {
      title = 'черновик';
      bubbleInner = `
        <p class="draft-bubble-text">${esc(d.response_text)}</p>
        <div class="draft-bubble-actions">
          <button class="draft-text-btn" data-action="edit_enter"   data-id="${d.draft_id}">✏ править</button>
          <button class="draft-text-btn" data-action="refine_enter" data-id="${d.draft_id}">💬 уточнить</button>
          <button class="draft-text-btn" data-action="regenerate"   data-id="${d.draft_id}">🔄 ещё</button>
        </div>`;
      sendRow = `
        <button class="draft-trash-btn" data-action="reject"  data-id="${d.draft_id}" title="Отклонить">🗑</button>
        <button class="draft-send-btn"  data-action="approve" data-id="${d.draft_id}">↑ отправить</button>`;
    }

    panel.innerHTML = `
      <div class="draft-panel">
        <div class="draft-panel-header">
          <div class="draft-panel-label">
            <div class="draft-panel-dot">✨</div>
            <span class="draft-panel-title">${title}</span>
          </div>
          <div style="display:flex;gap:2px;">
            <button class="draft-icon-btn" data-action="thumb_up"   data-id="${d.draft_id}" title="Нравится">👍</button>
            <button class="draft-icon-btn" data-action="thumb_down" data-id="${d.draft_id}" title="Не нравится">👎</button>
          </div>
        </div>

        ${sourceHtml}

        <div class="draft-bubble">${bubbleInner}</div>

        <div class="draft-send-row">${sendRow}</div>
      </div>
    `;

    if (mode === 'editing') {
      const ta = panel.querySelector('#draft-edit-input');
      ta.focus();
      ta.selectionStart = ta.selectionEnd = ta.value.length;
    } else if (mode === 'refining') {
      panel.querySelector('#draft-refine-input')?.focus();
    }

    panel.querySelectorAll('[data-action]').forEach(btn => {
      btn.addEventListener('click', () => {
        const action = btn.dataset.action;
        const id = btn.dataset.id;
        switch (action) {
          case 'edit_enter':
            twa?.HapticFeedback?.impactOccurred('light');
            renderDraftView(panel, d, 'editing');
            break;
          case 'refine_enter':
            twa?.HapticFeedback?.impactOccurred('light');
            renderDraftView(panel, d, 'refining');
            break;
          case 'mode_normal':
            renderDraftView(panel, d, 'normal');
            break;
          case 'save_edit': {
            const text = panel.querySelector('#draft-edit-input')?.value?.trim();
            if (text) handleDraftAction(id, 'edit', text);
            break;
          }
          case 'do_refine': {
            const hint = panel.querySelector('#draft-refine-input')?.value?.trim();
            if (hint) handleDraftAction(id, 'ai_refine', hint);
            break;
          }
          default:
            handleDraftAction(id, action);
        }
      });
    });
  }

  async function handleDraftAction(draftId, action, editedText) {
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
    await executeDraftAction(draftId, action, editedText);
  }

  async function executeDraftAction(draftId, action, editedText) {
    el.querySelectorAll(`[data-id="${draftId}"]`).forEach(b => { b.disabled = true; b.style.opacity = '0.5'; });
    try {
      await api.draftAction(chatId, draftId, action, editedText);
      twa?.HapticFeedback?.notificationOccurred('success');
      await loadChatData();
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      el.querySelectorAll(`[data-id="${draftId}"]`).forEach(b => { b.disabled = false; b.style.opacity = ''; });
    }
  }

  // ── Data loaders ──────────────────────────────────────────────────────────

  async function loadChatData() {
    try {
      const [msgData, draftData] = await Promise.all([api.messages(chatId), api.drafts(chatId)]);
      renderMessages(msgData.messages || [], draftData.drafts || []);
      renderDraftPanel(draftData.drafts || []);
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
    new_draft:    (d) => { if (d.chat_id === chatId) loadChatData(); },
    draft_status: (d) => {
      if (currentDrafts.find(x => x.draft_id === d.draft_id)) loadChatData();
    },
    new_message:  (d) => { if (d.chat_id === chatId) loadChatData(); },
    ai_thinking:  (d) => { if (d.chat_id === chatId) showThinkingIndicator(); },
    ai_done:      (d) => { if (d.chat_id === chatId) hideThinkingIndicator(); },
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
  loadChatData();
  loadAvatar();

  return el;
}
