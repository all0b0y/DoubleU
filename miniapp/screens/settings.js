import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

const MODE_INFO = {
  disabled:  { label: 'disabled',  desc: 'DoubleU не работает в этом чате' },
  silent:    { label: 'silent',    desc: 'только анализ, без черновиков' },
  manual:    { label: 'manual',    desc: 'черновики только по запросу' },
  semi_auto: { label: 'semi_auto', desc: 'генерит, ты подтверждаешь' },
  auto:      { label: 'auto',      desc: 'отвечает самостоятельно' },
};
const MODE_ORDER = ['disabled', 'silent', 'manual', 'semi_auto', 'auto'];

// Privacy flags shown as positive toggles ("вкл" = удобное состояние), so each
// row documents what the flag actually controls. `inverted` means the toggle
// is ON when the flag is ABSENT.
const PRIVACY_TOGGLES = [
  { flag: 'no_memory',   icon: '🧠', title: 'память',          desc: 'запоминать факты из переписки', inverted: true },
  { flag: 'no_training', icon: '🎓', title: 'обучение стилю',  desc: 'учиться на твоих правках и ответах', inverted: true },
  { flag: 'no_summary',  icon: '📊', title: 'сводки',          desc: 'разрешить суммаризацию этого чата', inverted: true },
  { flag: 'private',     icon: '🔒', title: 'приватный чат',   desc: 'никаких автоответов — только вручную', inverted: false },
];

const FACT_TYPE_LABELS = {
  preference: 'предпочтение', biographical: 'факт', commitment: 'обязательство',
  event: 'событие', relationship: 'отношения', health: 'здоровье', taboo: 'табу',
};

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

export function renderSettings(navigate, chatId) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">настройки чата</div>
        <div class="chat-topbar-mode" id="st-chatname" style="color:var(--hint)">…</div>
      </div>
    </div>
    <div class="scroll" id="st-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;

  const handleBack = () => navigate(`/chat/${chatId}`);
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);

  let currentMode = 'manual';
  let currentFlags = [];
  let availableModes = new Set(['disabled', 'silent', 'manual']);
  let contextScore = 0;

  // ── Section renderers ───────────────────────────────────────────────────

  function modeRowHTML(mode) {
    const info = MODE_INFO[mode] || { label: mode, desc: '' };
    const isActive  = currentMode === mode;
    const isLocked  = !availableModes.has(mode);

    if (isLocked) {
      const threshold = mode === 'auto' ? 80 : 50;
      return `
        <div class="st-mode-row st-mode-locked">
          <span class="st-mode-icon">🔒</span>
          <div class="st-mode-body">
            <p class="st-mode-name st-mode-name-locked">${info.label}</p>
            <p class="st-mode-desc">${info.desc}</p>
          </div>
          <span class="st-mode-lock-badge">${contextScore} / ${threshold}</span>
        </div>`;
    }
    return `
      <div class="st-mode-row ${isActive ? 'st-mode-active' : ''}" data-mode="${mode}">
        <span class="st-mode-icon">${isActive ? '◉' : '○'}</span>
        <div class="st-mode-body">
          <p class="st-mode-name ${isActive ? 'st-mode-name-active' : ''}">${info.label}</p>
          <p class="st-mode-desc">${info.desc}</p>
        </div>
      </div>`;
  }

  // Role is bound to the bot now (one role governs all its chats), so this is an
  // informational pointer rather than a per-chat picker.
  function roleSectionHTML() {
    return `
      <div class="st-section">
        <p class="st-section-label">роль ИИ</p>
        <div class="st-card">
          <div class="st-row" style="cursor:default;">
            <span class="st-row-icon">🎭</span>
            <div class="st-row-body">
              <p class="st-row-title">роль задаётся для бота</p>
              <p class="st-row-desc">Личный кабинет → Боты → роль бота. Она применяется ко всем чатам бота.</p>
            </div>
          </div>
        </div>
      </div>`;
  }

  function privacySectionHTML() {
    return `
      <div class="st-section">
        <p class="st-section-label">приватность</p>
        <div class="st-card">
          ${PRIVACY_TOGGLES.map((t, i) => {
            const has = currentFlags.includes(t.flag);
            const on  = t.inverted ? !has : has;
            return `
              <div class="${i < PRIVACY_TOGGLES.length - 1 ? 'st-divider' : ''}">
                <div class="st-row" style="cursor:default;">
                  <span class="st-row-icon">${t.icon}</span>
                  <div class="st-row-body">
                    <p class="st-row-title">${t.title}</p>
                    <p class="st-row-desc">${t.desc}</p>
                  </div>
                  <label class="toggle">
                    <input type="checkbox" data-flag="${t.flag}" ${on ? 'checked' : ''}>
                    <div class="toggle-track"></div>
                    <div class="toggle-knob"></div>
                  </label>
                </div>
              </div>`;
          }).join('')}
        </div>
      </div>`;
  }

  function renderContent(settings) {
    currentMode  = settings.mode || 'manual';
    currentFlags = settings.privacy_flags || [];
    availableModes = new Set(settings.available_modes || ['disabled', 'silent', 'manual']);
    contextScore = settings.context_score ?? 0;

    el.querySelector('#st-scroll').innerHTML = `
      <div class="st-content">

        <!-- режим автоматизации -->
        <div class="st-section">
          <p class="st-section-label">режим автоматизации</p>
          <div class="st-card" id="mode-list">
            ${MODE_ORDER.map((m, i) => `
              <div class="st-card-divider-wrap ${i < MODE_ORDER.length - 1 ? 'st-divider' : ''}">
                ${modeRowHTML(m)}
              </div>`).join('')}
          </div>
        </div>

        <!-- роль ИИ -->
        <div id="role-section">${roleSectionHTML()}</div>

        <!-- приватность -->
        <div id="privacy-section">${privacySectionHTML()}</div>

        <!-- сводка -->
        <div class="st-section">
          <p class="st-section-label">сводка</p>
          <div class="st-card">
            <div class="st-row st-row-link" id="btn-summary">
              <span class="st-row-icon">📊</span>
              <div class="st-row-body">
                <p class="st-row-title">сводка чата</p>
                <p class="st-row-desc">ИИ перескажет последние сообщения</p>
              </div>
              <span class="st-chevron">›</span>
            </div>
          </div>
          <div id="summary-result"></div>
        </div>

        <!-- память -->
        <div class="st-section">
          <p class="st-section-label">память ИИ об этом чате</p>
          <div class="st-card">
            <div class="st-divider">
              <div class="st-row st-row-link" id="btn-facts">
                <span class="st-row-icon">🧠</span>
                <div class="st-row-body">
                  <p class="st-row-title">факты <span id="facts-count" style="color:var(--muted)"></span></p>
                  <p class="st-row-desc">что ИИ запомнил из переписки</p>
                </div>
                <span class="st-chevron" id="facts-chevron">›</span>
              </div>
              <div id="facts-list"></div>
            </div>
            <div>
              <div class="st-row st-row-link" id="btn-episodes">
                <span class="st-row-icon">📚</span>
                <div class="st-row-body">
                  <p class="st-row-title">сводки эпизодов <span id="episodes-count" style="color:var(--muted)"></span></p>
                  <p class="st-row-desc">архив завершённых разговоров</p>
                </div>
                <span class="st-chevron" id="episodes-chevron">›</span>
              </div>
              <div id="episodes-list"></div>
            </div>
          </div>
        </div>

        <!-- дополнительно -->
        <div class="st-section">
          <p class="st-section-label">дополнительно</p>
          <div class="st-card">
            <div class="st-divider">
              <div class="st-row st-row-link" id="btn-profile-link">
                <span class="st-row-icon">ⓘ</span>
                <div class="st-row-body">
                  <p class="st-row-title">досье собеседника</p>
                </div>
                <span class="st-chevron">›</span>
              </div>
            </div>
            <div>
              <div class="st-row st-row-danger" id="btn-forget">
                <span class="st-row-icon">🗑</span>
                <div class="st-row-body">
                  <p class="st-row-title" style="color:var(--danger)">стереть память чата</p>
                  <p class="st-row-desc">факты, сводки и история — чат и режим останутся</p>
                </div>
              </div>
            </div>
          </div>
        </div>

      </div>
    `;

    wireInteractions();
  }

  // ── Interactions ────────────────────────────────────────────────────────

  function wireInteractions() {
    // Mode selection — immediate save
    el.querySelectorAll('[data-mode]').forEach(row => {
      row.addEventListener('click', async () => {
        const mode = row.dataset.mode;
        if (mode === currentMode) return;
        twa?.HapticFeedback?.selectionChanged();
        currentMode = mode;
        el.querySelectorAll('#mode-list .st-card-divider-wrap').forEach((wrap, i) => {
          wrap.innerHTML = modeRowHTML(MODE_ORDER[i]);
        });
        wireInteractions();
        try {
          await api.updateSettings(chatId, { mode, privacy_flags: currentFlags });
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });

    // The role now lives on the bot (Личный кабинет → Боты) and governs every
    // chat, so the role section here is just a pointer — nothing to wire.

    // Privacy toggles — immediate save
    el.querySelectorAll('[data-flag]').forEach(input => {
      input.addEventListener('change', async () => {
        twa?.HapticFeedback?.selectionChanged();
        const t = PRIVACY_TOGGLES.find(x => x.flag === input.dataset.flag);
        const flagActive = t.inverted ? !input.checked : input.checked;
        currentFlags = flagActive
          ? [...new Set([...currentFlags, t.flag])]
          : currentFlags.filter(f => f !== t.flag);
        try {
          await api.updateSettings(chatId, { mode: currentMode, privacy_flags: currentFlags });
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });

    // Summary on demand
    el.querySelector('#btn-summary')?.addEventListener('click', async () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      const box = el.querySelector('#summary-result');
      box.innerHTML = `<div class="summary-result" style="color:var(--hint);font-style:italic;">✨ ИИ читает переписку…</div>`;
      try {
        const res = await api.summarize(chatId);
        if (res.blocked_by_privacy) {
          box.innerHTML = `<div class="summary-result" style="color:var(--hint);">🔒 Сводки выключены для этого чата — включи «сводки» в разделе приватности.</div>`;
        } else if (res.no_messages) {
          box.innerHTML = `<div class="summary-result" style="color:var(--hint);">Пока нет сообщений для сводки.</div>`;
        } else if (res.unavailable) {
          box.innerHTML = `<div class="summary-result" style="color:var(--hint);">Сводка временно недоступна (${res.message_count} сообщений). Попробуй позже.</div>`;
        } else {
          box.innerHTML = `
            <div class="summary-result">
              ${esc(res.summary)}
              ${(res.action_items || []).length ? `
                <ul class="summary-result-items">
                  ${res.action_items.map(i => `<li>${esc(i)}</li>`).join('')}
                </ul>` : ''}
            </div>`;
        }
        twa?.HapticFeedback?.notificationOccurred('success');
      } catch (err) {
        box.innerHTML = `<div class="summary-result" style="color:var(--danger);">Ошибка: ${esc(err.message)}</div>`;
      }
    });

    // Facts viewer (expand on demand)
    let factsOpen = false;
    el.querySelector('#btn-facts')?.addEventListener('click', async () => {
      twa?.HapticFeedback?.impactOccurred('light');
      const list = el.querySelector('#facts-list');
      factsOpen = !factsOpen;
      el.querySelector('#facts-chevron').textContent = factsOpen ? '⌄' : '›';
      if (!factsOpen) { list.innerHTML = ''; return; }
      list.innerHTML = `<div class="loading-row" style="padding:12px;"><div class="spinner"></div></div>`;
      await loadFacts(list);
    });

    // Episode summaries viewer
    let episodesOpen = false;
    el.querySelector('#btn-episodes')?.addEventListener('click', async () => {
      twa?.HapticFeedback?.impactOccurred('light');
      const list = el.querySelector('#episodes-list');
      episodesOpen = !episodesOpen;
      el.querySelector('#episodes-chevron').textContent = episodesOpen ? '⌄' : '›';
      if (!episodesOpen) { list.innerHTML = ''; return; }
      list.innerHTML = `<div class="loading-row" style="padding:12px;"><div class="spinner"></div></div>`;
      try {
        const res = await api.episodes(chatId);
        const eps = res.episodes || [];
        el.querySelector('#episodes-count').textContent = eps.length ? `· ${eps.length}` : '';
        list.innerHTML = eps.length
          ? eps.map(e => `
              <div class="fact-row">
                <div class="fact-row-body">
                  <p class="fact-row-text">${esc(e.content)}</p>
                  <div class="fact-row-meta">
                    <span class="fact-type-badge">${new Date(e.period_end).toLocaleDateString('ru', { day: 'numeric', month: 'short' })} · ${e.message_count} сообщ.</span>
                  </div>
                </div>
              </div>`).join('')
          : `<div style="padding:8px 12px 12px;font-size:12px;color:var(--muted);">Эпизодов пока нет — они появляются после завершённых разговоров.</div>`;
      } catch (err) {
        list.innerHTML = `<div style="padding:8px 12px 12px;font-size:12px;color:var(--danger);">Ошибка: ${esc(err.message)}</div>`;
      }
    });

    // Досье
    el.querySelector('#btn-profile-link')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('light');
      navigate(`/chat/${chatId}/profile`);
    });

    // Стереть память чата — real ForgetChat call
    el.querySelector('#btn-forget')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      twa?.showPopup({
        title: 'Стереть память чата?',
        message: 'Факты, сводки, черновики и история сообщений будут удалены безвозвратно. Сам чат и его режим останутся.',
        buttons: [
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
          { id: 'forget', type: 'destructive', text: 'Стереть' },
        ],
      }, async (btnId) => {
        if (btnId !== 'forget') return;
        try {
          await api.forgetChat(chatId);
          twa?.HapticFeedback?.notificationOccurred('success');
          navigate(`/chat/${chatId}`);
        } catch (err) {
          twa?.HapticFeedback?.notificationOccurred('error');
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });
  }

  async function loadFacts(list) {
    try {
      const res = await api.facts(chatId);
      const facts = res.facts || [];
      el.querySelector('#facts-count').textContent = facts.length ? `· ${facts.length}` : '';
      if (!facts.length) {
        list.innerHTML = `<div style="padding:8px 12px 12px;font-size:12px;color:var(--muted);">ИИ пока ничего не запомнил из этого чата.</div>`;
        return;
      }
      list.innerHTML = facts.map(f => `
        <div class="fact-row" data-fact-id="${f.id}">
          <div class="fact-row-body">
            <p class="fact-row-text">${esc(f.fact_text)}</p>
            <div class="fact-row-meta">
              <span class="fact-type-badge ${esc(f.fact_type)}">${FACT_TYPE_LABELS[f.fact_type] || esc(f.fact_type)}</span>
            </div>
          </div>
          <button class="fact-delete-btn" data-del-fact="${f.id}" title="Забыть">✕</button>
        </div>`).join('');

      list.querySelectorAll('[data-del-fact]').forEach(btn => {
        btn.addEventListener('click', () => {
          twa?.HapticFeedback?.impactOccurred('medium');
          twa?.showPopup({
            title: 'Забыть факт?',
            message: 'ИИ больше не будет учитывать его в ответах.',
            buttons: [
              { id: 'cancel', type: 'cancel', text: 'Отмена' },
              { id: 'del', type: 'destructive', text: 'Забыть' },
            ],
          }, async (btnId) => {
            if (btnId !== 'del') return;
            try {
              await api.deleteFact(btn.dataset.delFact);
              twa?.HapticFeedback?.notificationOccurred('success');
              await loadFacts(list);
            } catch (err) {
              twa?.showAlert(`Ошибка: ${err.message}`);
            }
          });
        });
      });
    } catch (err) {
      list.innerHTML = `<div style="padding:8px 12px 12px;font-size:12px;color:var(--danger);">Ошибка: ${esc(err.message)}</div>`;
    }
  }

  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  // Load chat name + settings
  Promise.all([api.chats(), api.chatSettings(chatId)]).then(([chatsData, settings]) => {
    const chat = chatsData.chats?.find(c => c.chat_id === chatId);
    if (chat) el.querySelector('#st-chatname').textContent = chat.display_name || chatId;
    renderContent(settings);
  }).catch(err => {
    el.querySelector('#st-scroll').innerHTML = `
      <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${esc(err.message)}</div></div>`;
  });

  return el;
}
