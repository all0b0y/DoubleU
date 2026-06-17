import { api, openEvents } from '/miniapp/api/client.js';
import { catImg } from '/miniapp/components/glyphs.js';

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
      <button class="chat-topbar-btn" id="btn-settings" title="Настройки">⚙</button>
    </div>

    <div class="scroll" id="msg-list">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>

    <div id="draft-panel"></div>

    <div class="chat-composer" id="chat-composer"
         style="display:flex;gap:8px;align-items:flex-end;padding:8px 12px calc(8px + var(--inset-bottom,0px));border-top:1px solid var(--border-color);background:var(--bg);">
      <textarea id="composer-input" rows="1" placeholder="Ответить от имени бота…"
        style="flex:1;resize:none;max-height:120px;background:var(--bg2);color:var(--text);border:1px solid var(--border-color);border-radius:18px;padding:9px 14px;font-size:14px;line-height:1.3;"></textarea>
      <button id="composer-send" title="Отправить" disabled
        style="flex-shrink:0;width:40px;height:40px;border:none;border-radius:50%;background:var(--accent);color:#fff;font-size:18px;cursor:pointer;">↑</button>
    </div>
  `;

  const handleBack = () => navigate('/chats');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);

  el.querySelector('#btn-settings').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate(`/chat/${chatId}/settings`);
  });

  // ── Composer: owner replies manually as the bot ────────────────────────────
  // The reply goes out through the bot and is stored as context the AI uses on
  // later drafts; any pending draft in this chat is discarded server-side.
  const composerInput = el.querySelector('#composer-input');
  const composerSend  = el.querySelector('#composer-send');

  function autoGrowComposer() {
    composerInput.style.height = 'auto';
    composerInput.style.height = Math.min(composerInput.scrollHeight, 120) + 'px';
    composerSend.disabled = composerInput.value.trim() === '';
  }
  composerInput.addEventListener('input', autoGrowComposer);

  async function sendComposer() {
    const text = composerInput.value.trim();
    if (!text) return;
    composerSend.disabled = true;
    composerInput.disabled = true;
    twa?.HapticFeedback?.impactOccurred('medium');
    try {
      await api.sendMessage(chatId, text);
      composerInput.value = '';
      autoGrowComposer();
      twa?.HapticFeedback?.notificationOccurred('success');
      await loadChatData();
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
    } finally {
      composerInput.disabled = false;
    }
  }
  composerSend.addEventListener('click', sendComposer);
  composerInput.addEventListener('keydown', (e) => {
    // Enter sends; Shift+Enter inserts a newline.
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendComposer(); }
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
    el.querySelector('#ai-error-notice')?.remove(); // a fresh turn supersedes a prior error
    const phrase = THINKING_PHRASES[Math.floor(Math.random() * THINKING_PHRASES.length)];
    const div = document.createElement('div');
    div.id = 'ai-thinking-indicator';
    div.className = 'ai-thinking';
    div.innerHTML =
      `<span class="ai-thinking-text">${phrase}</span>` +
      `<span class="ai-thinking-dots"><span></span><span></span><span></span></span>`;
    const inner = list.querySelector('.msg-list-inner');
    (inner || list).appendChild(div);
    list.scrollTop = list.scrollHeight;
  }

  function hideThinkingIndicator() {
    el.querySelector('#ai-thinking-indicator')?.remove();
  }

  // Replace the thinking indicator with a retryable error notice when an AI
  // turn fails, so it never just disappears with nothing in its place.
  function showAIError(message) {
    hideThinkingIndicator();
    const list = el.querySelector('#msg-list');
    el.querySelector('#ai-error-notice')?.remove();
    const div = document.createElement('div');
    div.id = 'ai-error-notice';
    div.className = 'ai-error';
    div.textContent = message || 'ИИ не смог сформировать ответ. Попробуйте ещё раз.';
    const inner = list.querySelector('.msg-list-inner');
    (inner || list).appendChild(div);
    list.scrollTop = list.scrollHeight;
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
        <div class="draft-rate-row" style="display:flex;gap:6px;margin-top:6px;justify-content:flex-end">
          <button class="draft-text-btn" data-action="thumb_up"   data-id="${d.draft_id}" title="Нравится">👍</button>
          <button class="draft-text-btn" data-action="thumb_down" data-id="${d.draft_id}" title="Не нравится">👎</button>
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
        // Distinguish AI/app-sent replies from messages typed straight to the
        // bot in Telegram, so the owner can tell personal use apart.
        let footer;
        if (m.label) {
          footer = `<div class="msg-footer-out">
               <span class="msg-footer-du">✨ ${m.label}</span>
               <span class="msg-footer-time">${formatTime(m.created_at)} ✓✓</span>
             </div>`;
        } else {
          const who = m.source === 'telegram' ? 'Ты · в Telegram' : 'Ты · из приложения';
          footer = `<div class="msg-footer-out">
               <span class="msg-footer-du" style="opacity:0.6">${who}</span>
               <span class="msg-footer-time">${formatTime(m.created_at)}</span>
             </div>`;
        }
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
        const av = el.querySelector('#chat-avatar');
        av.classList.add('avatar-cat');
        av.innerHTML = catImg(chatId);
      }
      if (settingsData) {
        const modeEl = el.querySelector('#chat-mode');
        const modeText = MODE_LABELS[settingsData.mode] || settingsData.mode;
        modeEl.textContent = settingsData.ai_role
          ? `${modeText} · 🎭 ${settingsData.ai_role.name}`
          : modeText;
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
    new_message:  (d) => { if (d.chat_id === chatId) { el.querySelector('#ai-error-notice')?.remove(); loadChatData(); } },
    ai_thinking:  (d) => { if (d.chat_id === chatId) showThinkingIndicator(); },
    ai_done:      (d) => { if (d.chat_id === chatId) hideThinkingIndicator(); },
    ai_error:     (d) => { if (d.chat_id === chatId) showAIError(d.message); },
    chat_role_changed: (d) => { if (d.chat_id === chatId) loadChatMeta(); },
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
