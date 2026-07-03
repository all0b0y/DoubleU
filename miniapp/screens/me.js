import { api } from '/miniapp/api/client.js';
import { renderBottomNav } from '/miniapp/components/nav.js';
import { chooseBotRoleContextPolicy } from '/miniapp/domain/bot-role-context-policy.js';

const twa = window.Telegram?.WebApp;

// Phrase must match contracts.ForgetAllConfirmationPhrase on the backend.
const FORGET_ALL_PHRASE = 'УДАЛИТЬ ВСЁ';

const MODE_OPTIONS = [
  { value: 'off',       label: 'выключен' },
  { value: 'observe',   label: 'наблюдение' },
  { value: 'manual',    label: 'manual — черновики по запросу' },
  { value: 'semi_auto', label: 'semi_auto — ты подтверждаешь' },
  { value: 'auto',      label: 'auto — отвечает сам' },
];

// Curated list of major IANA zones for the manual override — kept short on
// purpose (full tz database is 400+ entries and unusable in a <select>).
// The owner's actual auto-detected zone is injected at render time even when
// it isn't in this list, so the dropdown never silently shows the wrong value.
const TIMEZONE_OPTIONS = [
  ['UTC', 'UTC'],
  ['Europe/Kaliningrad', 'Калининград'],
  ['Europe/Kyiv', 'Киев'],
  ['Europe/Minsk', 'Минск'],
  ['Europe/Moscow', 'Москва'],
  ['Europe/Samara', 'Самара'],
  ['Asia/Yekaterinburg', 'Екатеринбург'],
  ['Asia/Almaty', 'Алматы'],
  ['Asia/Omsk', 'Омск'],
  ['Asia/Krasnoyarsk', 'Красноярск'],
  ['Asia/Irkutsk', 'Иркутск'],
  ['Asia/Yakutsk', 'Якутск'],
  ['Asia/Vladivostok', 'Владивосток'],
  ['Asia/Magadan', 'Магадан'],
  ['Asia/Kamchatka', 'Камчатка'],
  ['Asia/Tashkent', 'Ташкент'],
  ['Asia/Bishkek', 'Бишкек'],
  ['Asia/Dushanbe', 'Душанбе'],
  ['Asia/Baku', 'Баку'],
  ['Asia/Yerevan', 'Ереван'],
  ['Asia/Tbilisi', 'Тбилиси'],
  ['Europe/Warsaw', 'Варшава'],
  ['Europe/Berlin', 'Берлин'],
  ['Europe/London', 'Лондон'],
  ['Europe/Istanbul', 'Стамбул'],
  ['Asia/Dubai', 'Дубай'],
  ['Asia/Bangkok', 'Бангкок'],
  ['Asia/Shanghai', 'Пекин/Шанхай'],
  ['Asia/Tokyo', 'Токио'],
  ['America/New_York', 'Нью-Йорк'],
  ['America/Los_Angeles', 'Лос-Анджелес'],
];

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// Renders <option>s for the timezone <select>, injecting the current value
// when it falls outside the curated list so the dropdown is never wrong.
function timezoneOptionsHTML(current) {
  const known = TIMEZONE_OPTIONS.some(([iana]) => iana === current);
  const all = current && !known ? [[current, current], ...TIMEZONE_OPTIONS] : TIMEZONE_OPTIONS;
  return all.map(([iana, label]) => `
    <option value="${esc(iana)}" ${iana === current ? 'selected' : ''}>${esc(label)} (${esc(iana)})</option>
  `).join('');
}

function suggestionCardHTML(s) {
  return `
    <div class="suggestion-card" data-suggestion-id="${s.id}">
      <p class="suggestion-card-title">
        💡 <b>${esc(s.chat_display_name)}</b>: ${esc(s.field_label)} → <b>${esc(s.suggested_value)}</b>
      </p>
      ${s.rationale ? `<p class="suggestion-card-rationale">${esc(s.rationale)}</p>` : ''}
      <div class="suggestion-card-actions">
        <button class="suggestion-btn accept" data-sg-action="accept">✅ Принять</button>
        <button class="suggestion-btn reject" data-sg-action="reject">✖ Отклонить</button>
      </div>
    </div>`;
}

// telegramNick returns the owner's Telegram handle for the cabinet header:
// @username when set, else the first name, else a neutral fallback.
function telegramNick() {
  const user = twa?.initDataUnsafe?.user;
  if (user?.username) return '@' + user.username;
  if (user?.first_name) return esc(user.first_name);
  return 'Личный кабинет';
}

export function renderMe(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chats-header">
      <div class="chats-header-left">
        <span class="chats-header-brand">Личный кабинет</span>
        <span class="chats-header-title">${telegramNick()}</span>
      </div>
    </div>
    <div class="scroll" id="me-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
  `;
  el.appendChild(renderBottomNav('me', navigate));

  twa?.BackButton?.hide();

  let settings = {};
  let roles = [];
  // The me-screen bot section is single-bot; with multibot we show the tenant's
  // first bot. Kept here so the role-picker handler (in wire()) has its bot id.
  let currentBot = { connected: false };

  function personalConnectionHTML(me) {
    const status = me?.connection?.status || 'not_found';
    const dot = status === 'connected' ? 'ok' : status === 'revoked' ? 'bad' : 'idle';
    const label = status === 'connected' ? 'подключено'
      : status === 'revoked' ? 'доступ отозван' : 'не подключено';
    return `
      <div class="st-row" style="cursor:default;">
        <span class="st-row-icon"><span class="status-dot ${dot}"></span></span>
        <div class="st-row-body">
          <p class="st-row-title">Telegram</p>
          <p class="st-row-desc">${label}${me?.connection?.can_reply === false && status === 'connected' ? ' · без права отвечать' : ''}</p>
        </div>
      </div>`;
  }

  function botConnectionHTML(bot) {
    if (!bot?.connected) {
      return `
        <div class="st-row" style="cursor:default;">
          <span class="st-row-icon"><span class="status-dot idle"></span></span>
          <div class="st-row-body">
            <p class="st-row-title">Бот</p>
            <p class="st-row-desc">не подключён</p>
          </div>
        </div>`;
    }
    const ok = bot.status === 'active' || !bot.status;
    return `
      <div class="st-row" style="cursor:default;">
        <span class="st-row-icon"><span class="status-dot ${ok ? 'ok' : 'bad'}"></span></span>
        <div class="st-row-body">
          <p class="st-row-title">@${esc(bot.bot_username || bot.bot_id)}</p>
          <p class="st-row-desc">${ok ? 'подключён' : esc(bot.status)}</p>
        </div>
      </div>`;
  }

  // The bot's role governs every chat of that bot — assigned here, not per chat.
  function botRolePickerHTML(bot) {
    if (!bot?.connected) return '';
    const current = bot.ai_role?.id || 0;
    return `
      <div class="st-divider"></div>
      <div class="field-group" style="padding:12px 14px;">
        <div class="field-label">роль бота</div>
        <select class="field-select" id="f-bot-role">
          <option value="0" ${!current ? 'selected' : ''}>без роли — обычный ассистент</option>
          ${roles.map(r => `
            <option value="${r.id}" ${current === r.id ? 'selected' : ''}>${esc(r.name)}</option>
          `).join('')}
        </select>
        <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">
          Эта роль применяется ко всем чатам бота.
        </div>
      </div>`;
  }

  function renderContent(me, suggestions, bot) {
    el.querySelector('#me-scroll').innerHTML = `
      <div class="st-content">

        ${suggestions.length ? `
        <div class="st-section">
          <p class="st-section-label">ИИ предлагает обновить профили</p>
          <div class="st-card" id="sg-list">
            ${suggestions.map((s, i) => `
              <div class="${i < suggestions.length - 1 ? 'st-divider' : ''}">${suggestionCardHTML(s)}</div>
            `).join('')}
          </div>
          <div style="font-size:11px;color:var(--muted);padding:6px 4px 0;line-height:1.4;">
            ИИ никогда не меняет досье сам — только с твоего согласия.
          </div>
        </div>` : ''}

        <div class="st-section">
          <p class="st-section-label">поведение по умолчанию</p>
          <div class="st-card" style="padding:14px;display:flex;flex-direction:column;gap:14px;">
            <div class="field-group">
              <div class="field-label">режим для новых чатов</div>
              <select class="field-select" id="f-default-mode">
                ${MODE_OPTIONS.map(o => `
                  <option value="${o.value}" ${settings.default_mode_for_new_chats === o.value ? 'selected' : ''}>${o.label}</option>
                `).join('')}
              </select>
            </div>
            <div class="field-group">
              <div class="field-label">язык черновиков</div>
              <select class="field-select" id="f-draft-lang">
                <option value="auto"    ${settings.language_for_drafts !== 'primary' ? 'selected' : ''}>как у собеседника</option>
                <option value="primary" ${settings.language_for_drafts === 'primary' ? 'selected' : ''}>всегда мой язык</option>
              </select>
            </div>
            <div class="field-group">
              <div class="field-label">часовой пояс</div>
              <select class="field-select" id="f-timezone">
                ${timezoneOptionsHTML(settings.timezone || 'UTC')}
              </select>
              <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">
                Определяется автоматически по устройству — используется для напоминаний. Можно задать вручную.
              </div>
            </div>
            <div class="st-row" style="padding:0;cursor:default;">
              <div class="st-row-body">
                <p class="st-row-title">учиться на моих правках</p>
                <p class="st-row-desc">черновики становятся точнее со временем</p>
              </div>
              <label class="toggle">
                <input type="checkbox" id="f-learning" ${settings.learning_enabled ? 'checked' : ''}>
                <div class="toggle-track"></div>
                <div class="toggle-knob"></div>
              </label>
            </div>
            <div class="st-row" style="padding:0;cursor:default;">
              <div class="st-row-body">
                <p class="st-row-title">rich-форматирование ответов</p>
                <p class="st-row-desc">таблицы, списки и код для ролей</p>
              </div>
              <label class="toggle">
                <input type="checkbox" id="f-rich-formatting" ${settings.rich_formatting_enabled !== false ? 'checked' : ''}>
                <div class="toggle-track"></div>
                <div class="toggle-knob"></div>
              </label>
            </div>
            <div class="st-row" style="padding:0;cursor:default;">
              <div class="st-row-body">
                <p class="st-row-title">потоковый AI-режим</p>
                <p class="st-row-desc">показывать набор готового ответа</p>
              </div>
              <label class="toggle">
                <input type="checkbox" id="f-streaming" ${settings.streaming_enabled ? 'checked' : ''}>
                <div class="toggle-track"></div>
                <div class="toggle-knob"></div>
              </label>
            </div>
          </div>
          <div style="padding:10px 0 0;">
            <button class="btn btn-secondary" id="btn-save-me">Сохранить</button>
          </div>
        </div>

        <div class="st-section">
          <p class="st-section-label">конфигурация ролей</p>
          <div class="st-card">
            <div class="st-row st-row-link" id="btn-nav-skills">
              <span class="st-row-icon">✦</span>
              <div class="st-row-body">
                <p class="st-row-title">навыки</p>
                <p class="st-row-desc">инструкции, которые роль подгружает по теме</p>
              </div>
            </div>
            <div class="st-row st-row-link" id="btn-nav-mcp">
              <span class="st-row-icon">↗</span>
              <div class="st-row-body">
                <p class="st-row-title">внешние инструменты (MCP)</p>
                <p class="st-row-desc">серверы инструментов, доступные ролям</p>
              </div>
            </div>
          </div>
        </div>

        <div class="st-section">
          <p class="st-section-label">опасная зона</p>
          <div class="st-card">
            <div class="st-row st-row-danger" id="btn-forget-all">
              <span class="st-row-icon">🗑</span>
              <div class="st-row-body">
                <p class="st-row-title" style="color:var(--danger)">удалить все данные</p>
                <p class="st-row-desc">чаты, память, роли и настройки — безвозвратно</p>
              </div>
            </div>
            <div id="forget-all-confirm" style="display:none;padding:0 12px 12px;">
              <p style="font-size:12px;color:var(--hint);line-height:1.5;margin:0 0 8px;">
                Для подтверждения введи фразу <b style="color:var(--danger)">${FORGET_ALL_PHRASE}</b>:
              </p>
              <input class="danger-confirm-input" id="forget-all-input" placeholder="${FORGET_ALL_PHRASE}">
              <button class="btn btn-danger" id="btn-forget-all-go" style="margin-top:8px;" disabled>
                Удалить всё навсегда
              </button>
            </div>
          </div>
        </div>

        <div class="st-section">
          <p class="st-section-label">личное подключение</p>
          <div class="st-card">${personalConnectionHTML(me)}</div>
        </div>

        <div class="st-section">
          <p class="st-section-label">боты</p>
          <div class="st-card">${botConnectionHTML(bot)}${botRolePickerHTML(bot)}</div>
        </div>

      </div>`;

    wire();
  }

  function wire() {
    // Bot role: applies to every chat of the bot (saved instantly on change).
    el.querySelector('#f-bot-role')?.addEventListener('change', async (e) => {
      twa?.HapticFeedback?.selectionChanged();
      const select = e.target;
      const roleId = Number(select.value);
      const previousRoleId = currentBot.ai_role?.id || 0;
      const roleName = roleId
        ? roles.find(r => Number(r.id) === roleId)?.name || 'выбранная роль'
        : 'обычный ассистент';
      const roleContextPolicy = await chooseBotRoleContextPolicy(twa, roleName);
      if (!roleContextPolicy) {
        select.value = String(previousRoleId);
        return;
      }
      try {
        currentBot = await api.setBotRole(currentBot.bot_id, roleId, roleContextPolicy);
        twa?.HapticFeedback?.notificationOccurred('success');
      } catch (err) {
        select.value = String(previousRoleId);
        twa?.HapticFeedback?.notificationOccurred('error');
        twa?.showAlert(`Ошибка: ${err.message}`);
        load();
      }
    });

    // Suggestion accept/reject
    el.querySelectorAll('[data-suggestion-id]').forEach(card => {
      card.querySelectorAll('[data-sg-action]').forEach(btn => {
        btn.addEventListener('click', async () => {
          twa?.HapticFeedback?.impactOccurred('medium');
          card.querySelectorAll('button').forEach(b => { b.disabled = true; b.style.opacity = '0.5'; });
          try {
            await api.suggestionAction(card.dataset.suggestionId, btn.dataset.sgAction);
            twa?.HapticFeedback?.notificationOccurred('success');
            load();
          } catch (err) {
            twa?.showAlert(`Ошибка: ${err.message}`);
            card.querySelectorAll('button').forEach(b => { b.disabled = false; b.style.opacity = ''; });
          }
        });
      });
    });

    // Save user settings
    el.querySelector('#btn-save-me')?.addEventListener('click', async () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      const btn = el.querySelector('#btn-save-me');
      btn.textContent = '…'; btn.disabled = true;
      try {
        settings = await api.saveUserSettings({
          // display_name/bio are no longer edited here; preserve stored values
          // so saving behaviour settings doesn't wipe the existing profile.
          display_name:               settings.display_name,
          bio:                        settings.bio,
          timezone:                   el.querySelector('#f-timezone').value,
          default_mode_for_new_chats: el.querySelector('#f-default-mode').value,
          language_for_drafts:        el.querySelector('#f-draft-lang').value,
          learning_enabled:           el.querySelector('#f-learning').checked,
          rich_formatting_enabled:    el.querySelector('#f-rich-formatting').checked,
          streaming_enabled:          el.querySelector('#f-streaming').checked,
        });
        twa?.HapticFeedback?.notificationOccurred('success');
        btn.textContent = '✓ Сохранено';
        setTimeout(() => { btn.textContent = 'Сохранить'; btn.disabled = false; }, 1200);
      } catch (err) {
        twa?.HapticFeedback?.notificationOccurred('error');
        twa?.showAlert(`Ошибка: ${err.message}`);
        btn.textContent = 'Сохранить'; btn.disabled = false;
      }
    });

    el.querySelector('#btn-nav-skills')?.addEventListener('click', () => navigate('/skills'));
    el.querySelector('#btn-nav-mcp')?.addEventListener('click', () => navigate('/mcp-servers'));

    // Forget-all: reveal the typed-phrase confirmation, same contract as the bot.
    el.querySelector('#btn-forget-all')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      const box = el.querySelector('#forget-all-confirm');
      box.style.display = box.style.display === 'none' ? 'block' : 'none';
    });
    const input = el.querySelector('#forget-all-input');
    const goBtn = el.querySelector('#btn-forget-all-go');
    input?.addEventListener('input', () => {
      goBtn.disabled = input.value.trim() !== FORGET_ALL_PHRASE;
    });
    goBtn?.addEventListener('click', async () => {
      twa?.HapticFeedback?.notificationOccurred('warning');
      goBtn.textContent = '…'; goBtn.disabled = true;
      try {
        await api.forgetAll(input.value.trim());
        // Everything is gone, including the connection — return to the splash.
        navigate('/');
      } catch (err) {
        twa?.showAlert(`Ошибка: ${err.message}`);
        goBtn.textContent = 'Удалить всё навсегда'; goBtn.disabled = false;
      }
    });
  }

  async function load() {
    try {
      const [me, settingsData, suggestionsData, botsData, rolesData] = await Promise.all([
        api.me(), api.userSettings(), api.suggestions(),
        api.bots().catch(() => ({ bots: [] })),
        api.roles().catch(() => ({ roles: [] })),
      ]);
      settings = settingsData || {};
      roles = rolesData.roles || [];
      currentBot = (botsData.bots && botsData.bots[0]) || { connected: false };
      renderContent(me, suggestionsData.suggestions || [], currentBot);
    } catch (err) {
      el.querySelector('#me-scroll').innerHTML = `
        <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${esc(err.message)}</div></div>`;
    }
  }

  load();
  return el;
}
