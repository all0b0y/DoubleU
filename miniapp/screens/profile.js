import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const TABS = [
  { id: 'contact', label: 'Собеседник' },
  { id: 'me',      label: 'Я' },
  { id: 'chat',    label: 'Чат' },
];

// Minimum character requirement for "Я" and "Чат" message fields before saving.
const MIN_MESSAGE_LENGTH = 20;

function parseNotes(raw) {
  try { return JSON.parse(raw || '{}'); } catch (_) { return {}; }
}
function serializeNotes(obj) { return JSON.stringify(obj); }

export function renderProfile(navigate, chatId) {
  let profile = { name: '', gender: '', role: '', relationship_type: '', notes: '' };
  let activeTab = 'contact';

  const el = document.createElement('div');
  el.className = 'screen';
  el.style.cssText = 'height:100%;display:flex;flex-direction:column;overflow:hidden;background:var(--bg);';

  el.innerHTML = `
    <div style="flex-shrink:0;padding:calc(14px + var(--inset-top, 0px)) 20px 0;">
      <div id="profile-name" style="font-size:18px;font-weight:600;color:var(--text);"></div>
    </div>

    <div class="tabs" id="tab-bar" style="margin-top:14px;">
      ${TABS.map(t => `
        <button class="tab-btn ${t.id === activeTab ? 'active' : ''}" data-tab="${t.id}">${t.label}</button>
      `).join('')}
    </div>

    <div class="scroll" id="tab-content" style="padding:20px;display:flex;flex-direction:column;gap:16px;">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>

    <div style="padding:12px 16px calc(12px + var(--inset-bottom,0px));flex-shrink:0;">
      <button id="btn-save" class="btn btn-primary">Сохранить</button>
    </div>
  `;

  function captureCurrentTab() {
    const notes = parseNotes(profile.notes);
    profile = {
      ...profile,
      name:              el.querySelector('#f-name')?.value              ?? profile.name,
      gender:            el.querySelector('#f-gender')?.value            ?? profile.gender,
      role:              el.querySelector('#f-role')?.value              ?? profile.role,
      relationship_type: el.querySelector('#f-reltype')?.value           ?? profile.relationship_type,
      notes: serializeNotes({
        ...notes,
        me_style:     el.querySelector('#f-me-style')?.value    ?? notes.me_style    ?? '',
        chat_context: el.querySelector('#f-chat-ctx')?.value    ?? notes.chat_context ?? '',
      }),
    };
  }

  // Tab clicks — capture current values before switching so nothing is lost (issue 6)
  el.querySelector('#tab-bar').addEventListener('click', (e) => {
    const btn = e.target.closest('[data-tab]');
    if (!btn || btn.dataset.tab === activeTab) return;
    twa?.HapticFeedback?.selectionChanged();
    captureCurrentTab();
    activeTab = btn.dataset.tab;
    el.querySelectorAll('.tab-btn').forEach(b => b.classList.toggle('active', b.dataset.tab === activeTab));
    renderTab();
  });

  function getFormValues() {
    captureCurrentTab();
    return profile;
  }

  function msgCounter(len) {
    const color = len > 0 && len < MIN_MESSAGE_LENGTH ? 'var(--error, #f44)' : 'var(--muted)';
    return `<span style="font-size:11px;color:${color};">${len}/${MIN_MESSAGE_LENGTH}</span>`;
  }

  function renderTab() {
    const content = el.querySelector('#tab-content');
    const notes = parseNotes(profile.notes);

    if (activeTab === 'contact') {
      content.innerHTML = `
        <div class="field-group">
          <div class="field-label">Как называть</div>
          <input class="field-input" id="f-name" placeholder="Имя или прозвище" value="${esc(profile.name)}">
        </div>
        <div class="field-group">
          <div class="field-label">Пол</div>
          <select class="field-select" id="f-gender">
            <option value="" ${!profile.gender?'selected':''}>Не указан</option>
            <option value="male"   ${profile.gender==='male'  ?'selected':''}>Мужской</option>
            <option value="female" ${profile.gender==='female'?'selected':''}>Женский</option>
            <option value="other"  ${profile.gender==='other' ?'selected':''}>Другой</option>
          </select>
        </div>
        <div class="field-group">
          <div class="field-label">Кто это</div>
          <input class="field-input" id="f-role" placeholder="Клиент, коллега, партнёр…" value="${esc(profile.role)}">
        </div>
        <div class="field-group">
          <div class="field-label">Вид отношений</div>
          <select class="field-select" id="f-reltype">
            <option value=""           ${!profile.relationship_type?'selected':''}>Не указан</option>
            <option value="personal"   ${profile.relationship_type==='personal'  ?'selected':''}>Личное</option>
            <option value="business"   ${profile.relationship_type==='business'  ?'selected':''}>Рабочее</option>
            <option value="friendship" ${profile.relationship_type==='friendship'?'selected':''}>Дружба</option>
            <option value="romantic"   ${profile.relationship_type==='romantic'  ?'selected':''}>Романтическое</option>
            <option value="family"     ${profile.relationship_type==='family'    ?'selected':''}>Семья</option>
          </select>
        </div>
      `;
    } else if (activeTab === 'me') {
      const meStyle = notes.me_style || '';
      content.innerHTML = `
        <div style="font-size:13px;color:var(--hint);line-height:1.5;">
          Как ты общаешься с этим человеком: тон, стиль, привычки.
        </div>
        <div class="field-group">
          <div class="field-label" style="display:flex;justify-content:space-between;align-items:center;">
            <span>Мой стиль</span>
            <span id="counter-me-style" style="font-size:11px;color:var(--muted);">${msgCounter(meStyle.length)}</span>
          </div>
          <textarea class="field-input field-textarea" id="f-me-style"
            placeholder="Неформально, шучу…"
          >${esc(meStyle)}</textarea>
        </div>
      `;
      el.querySelector('#f-me-style').addEventListener('input', (e) => {
        const counter = el.querySelector('#counter-me-style');
        if (counter) counter.innerHTML = msgCounter(e.target.value.length);
      });
    } else {
      const chatCtx = notes.chat_context || '';
      content.innerHTML = `
        <div style="font-size:13px;color:var(--hint);line-height:1.5;">
          О чём вы разговариваете, важные темы.
        </div>
        <div class="field-group">
          <div class="field-label" style="display:flex;justify-content:space-between;align-items:center;">
            <span>Контекст чата</span>
            <span id="counter-chat-ctx" style="font-size:11px;color:var(--muted);">${msgCounter(chatCtx.length)}</span>
          </div>
          <textarea class="field-input field-textarea" id="f-chat-ctx"
            placeholder="Проект X, менеджер…"
          >${esc(chatCtx)}</textarea>
        </div>
      `;
      el.querySelector('#f-chat-ctx').addEventListener('input', (e) => {
        const counter = el.querySelector('#counter-chat-ctx');
        if (counter) counter.innerHTML = msgCounter(e.target.value.length);
      });
    }
  }

  el.querySelector('#btn-save').addEventListener('click', async () => {
    twa?.HapticFeedback?.impactOccurred('medium');
    const data = getFormValues();
    const notes = parseNotes(data.notes);
    const meStyle = (notes.me_style || '').trim();
    const chatCtx = (notes.chat_context || '').trim();
    if (meStyle && meStyle.length < MIN_MESSAGE_LENGTH) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Поле "Мой стиль" должно содержать не менее ${MIN_MESSAGE_LENGTH} символов.`);
      return;
    }
    if (chatCtx && chatCtx.length < MIN_MESSAGE_LENGTH) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Поле "Контекст чата" должно содержать не менее ${MIN_MESSAGE_LENGTH} символов.`);
      return;
    }
    const btn = el.querySelector('#btn-save');
    btn.textContent = '…'; btn.disabled = true;
    try {
      await api.saveProfile(chatId, data);
      profile = { ...profile, ...data };
      twa?.HapticFeedback?.notificationOccurred('success');
      navigate(`/chat/${chatId}`);
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      btn.textContent = 'Сохранить'; btn.disabled = false;
    }
  });

  const handleBack = () => navigate(`/chat/${chatId}`);
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  api.getProfile(chatId).then(data => {
    profile = data;
    el.querySelector('#profile-name').textContent = data.name || '';
    renderTab();
  }).catch(() => renderTab());

  return el;
}

function esc(str) {
  return String(str || '').replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;');
}
