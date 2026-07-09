import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function esc(str) {
  return String(str || '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function nullableBoolMode(value) {
  if (value === true) return 'on';
  if (value === false) return 'off';
  return 'inherit';
}

function modeToNullableBool(mode) {
  if (mode === 'on') return true;
  if (mode === 'off') return false;
  return null;
}

/**
 * Create (roleId == null) or edit a role. The five labeled fields map 1:1 to
 * the ai_roles columns — a guided replacement for the old pipe-syntax command.
 */
export function renderRoleEdit(navigate, roleId) {
  const isNew = roleId === null;
  let role = {
    name: '', description: '', speaking_style: '', goals: '', constraints: '',
    context: '', custom_fields: [], rich_formatting_enabled: null, streaming_enabled: null,
    speak_as_owner: true, model_tier: 'balanced', model_name: '',
  };
  // Owner-defined extra fields (title + why + optional context), edited live.
  let customFields = [];

  // A template tap on the roles screen pre-fills the create form (one-shot).
  if (isNew) {
    const raw = sessionStorage.getItem('role_template');
    if (raw) {
      try { role = { ...role, ...JSON.parse(raw) }; } catch (_) { /* ignore */ }
      sessionStorage.removeItem('role_template');
    }
  }

  let modelsConfig = null;

  async function fetchModelsConfig() {
    try {
      modelsConfig = await api.modelConfig();
    } catch (_) { /* keep the saved role model when config is unavailable */ }
  }

  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">${isNew ? 'новая роль' : 'роль'}</div>
        <div class="chat-topbar-mode" id="re-subtitle" style="color:var(--hint)">
          ${isNew ? 'кем будет ИИ в чате' : '…'}
        </div>
      </div>
    </div>
    <div class="scroll" id="re-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
    <div style="padding:12px 16px calc(12px + var(--inset-bottom,0px));flex-shrink:0;display:flex;gap:8px;">
      <button id="btn-tools" class="btn btn-secondary">Инструменты</button>
      <button id="btn-save" class="btn btn-primary">${isNew ? 'Создать роль' : 'Сохранить'}</button>
    </div>
  `;

  const handleBack = () => navigate('/roles');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(handleBack);
  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      screenDisposed = true;
      if (recordingTimeout) clearTimeout(recordingTimeout);
      if (mediaRecorder?.state === 'recording') mediaRecorder.stop();
      audioStream?.getTracks().forEach(track => track.stop());
      twa?.BackButton?.offClick(handleBack);
      obs.disconnect();
    }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  function fieldHTML(id, label, hint, value, { textarea = false, required = false, placeholder = '' } = {}) {
    const input = textarea
      ? `<textarea class="field-input field-textarea" id="${id}" placeholder="${esc(placeholder)}">${esc(value)}</textarea>`
      : `<input class="field-input" id="${id}" placeholder="${esc(placeholder)}" maxlength="64" value="${esc(value)}">`;
    return `
      <div class="field-group">
        <div class="field-label">${label}${required ? ' *' : ''}</div>
        ${input}
        <div style="font-size:11px;color:var(--muted);line-height:1.4;">${hint}</div>
      </div>`;
  }

  function deliverySelectHTML(id, label, hint, value) {
    const mode = nullableBoolMode(value);
    return `
      <div class="field-group">
        <div class="field-label">${label}</div>
        <select class="field-select" id="${id}">
          <option value="inherit" ${mode === 'inherit' ? 'selected' : ''}>наследовать</option>
          <option value="on" ${mode === 'on' ? 'selected' : ''}>включено</option>
          <option value="off" ${mode === 'off' ? 'selected' : ''}>выключено</option>
        </select>
        <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">${hint}</div>
      </div>`;
  }

  // speak_as_owner toggle: ON = "личный чат" (AI replies as the owner, an
  // invisible human stand-in); OFF = "как бот" (AI replies openly as the role
  // persona). Mirrors AIRole.speak_as_owner on the backend.
  function speakAsSection() {
    const asOwner = role.speak_as_owner !== false; // default ON
    return `
      <div class="st-section">
        <p class="st-section-label">от чьего лица отвечать</p>
        <div class="st-card">
          <div class="st-row" style="cursor:default;">
            <span class="st-row-icon">🎭</span>
            <div class="st-row-body">
              <p class="st-row-title">Личный чат — от вашего лица</p>
              <p class="st-row-desc">Вкл: ИИ пишет как вы, собеседник думает, что общается с вами. Выкл: ИИ отвечает открыто как бот-ассистент в этой роли.</p>
            </div>
            <label class="toggle">
              <input type="checkbox" id="f-speak-as-owner" ${asOwner ? 'checked' : ''}>
              <div class="toggle-track"></div>
              <div class="toggle-knob"></div>
            </label>
          </div>
        </div>
      </div>`;
  }

  // auto_learn_profile toggle: ON (default) = AI-detected profile updates apply
  // automatically and are revertible from «Обо мне»; OFF = they wait for manual
  // approval. Mirrors AIRole.auto_learn_profile on the backend.
  function autoLearnSection() {
    const autoLearn = role.auto_learn_profile !== false; // default ON
    return `
      <div class="st-section">
        <p class="st-section-label">память профиля</p>
        <div class="st-card">
          <div class="st-row" style="cursor:default;">
            <span class="st-row-icon">🧠</span>
            <div class="st-row-body">
              <p class="st-row-title">Автообучение профиля собеседника</p>
              <p class="st-row-desc">Вкл: ИИ сам обновляет профиль (тон, обращение, язык…) по ходу общения; изменения можно откатить в «Обо мне». Выкл: обновления ждут вашего подтверждения.</p>
            </div>
            <label class="toggle">
              <input type="checkbox" id="f-auto-learn-profile" ${autoLearn ? 'checked' : ''}>
              <div class="toggle-track"></div>
              <div class="toggle-knob"></div>
            </label>
          </div>
        </div>
      </div>`;
  }

  // auto_improve_skills toggle: OFF (default) = skills improve only on your
  // request; ON = an autonomous curator rewrites skills the bot uses and you
  // dislike, as revertible versions. Mirrors AIRole.auto_improve_skills.
  function autoImproveSection() {
    const on = role.auto_improve_skills === true; // default OFF
    return `
      <div class="st-section">
        <p class="st-section-label">навыки</p>
        <div class="st-card">
          <div class="st-row" style="cursor:default;">
            <span class="st-row-icon">♻️</span>
            <div class="st-row-body">
              <p class="st-row-title">Автоулучшение навыков</p>
              <p class="st-row-desc">Вкл: ИИ сам дорабатывает навыки, которые часто применяются и получают негативную реакцию (изменения обратимы в «Навыках»). Выкл: только по вашей кнопке «Улучшить».</p>
            </div>
            <label class="toggle">
              <input type="checkbox" id="f-auto-improve-skills" ${on ? 'checked' : ''}>
              <div class="toggle-track"></div>
              <div class="toggle-knob"></div>
            </label>
          </div>
        </div>
      </div>`;
  }

  function deliverySettingsHTML() {
    return `
      <div class="st-section">
        <p class="st-section-label">формат ответов</p>
        <div class="st-card" style="padding:14px;display:flex;flex-direction:column;gap:14px;">
          ${deliverySelectHTML('f-rich-formatting', 'rich-форматирование', 'таблицы, списки и код в ответах этой роли', role.rich_formatting_enabled)}
          ${deliverySelectHTML('f-streaming', 'потоковый AI-режим', 'псевдо-стриминг готового безопасного ответа', role.streaming_enabled)}
        </div>
      </div>`;
  }

  const MODEL_TIERS = ['fast', 'balanced', 'smart'];

  function configuredModelOptions() {
    const seen = new Set();
    const options = [];
    const add = (model) => {
      const name = String(model || '').trim();
      if (!name || seen.has(name)) return;
      seen.add(name);
      options.push(name);
    };

    MODEL_TIERS.forEach(tier => {
      const models = Array.isArray(modelsConfig?.[tier]) ? modelsConfig[tier] : [];
      models.forEach(add);
    });
    add(modelsConfig?.default);
    return options;
  }

  function modelTierForName(modelName) {
    const name = String(modelName || '').trim();
    if (name) {
      for (const tier of MODEL_TIERS) {
        const models = Array.isArray(modelsConfig?.[tier]) ? modelsConfig[tier] : [];
        if (models.includes(name)) return tier;
      }
    }
    return MODEL_TIERS.includes(role.model_tier) ? role.model_tier : 'balanced';
  }

  function selectedModelName(options = configuredModelOptions()) {
    const saved = String(role.model_name || '').trim();
    if (saved) return saved;
    const configuredDefault = String(modelsConfig?.default || '').trim();
    if (configuredDefault) return configuredDefault;
    return options[0] || '';
  }

  function modelSettingsHTML() {
    const modelOptions = configuredModelOptions();
    const selectedModel = selectedModelName(modelOptions);
    const staleModelOption = selectedModel && !modelOptions.includes(selectedModel)
      ? `<option value="${esc(selectedModel)}" selected>${esc(selectedModel)} (недоступна)</option>`
      : '';
    const emptyOption = !modelOptions.length && !selectedModel
      ? '<option value="" selected disabled>Модели не настроены</option>'
      : '';
    return `
      <div class="st-section">
        <p class="st-section-label">модель ответов</p>
        <div class="st-card" style="padding:14px;display:flex;flex-direction:column;gap:14px;">
          <div class="field-group">
            <div class="field-label">модель</div>
            <select class="field-select" id="f-model-name">
              ${modelOptions.map(model => `
                <option value="${esc(model)}" ${selectedModel === model ? 'selected' : ''}>${esc(model)}</option>
              `).join('')}
              ${staleModelOption}
              ${emptyOption}
            </select>
            <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">
              Список берётся из модельных env-переменных сервера.
            </div>
          </div>
        </div>
      </div>`;
  }

  // One editable custom-field row (title + why + optional context + remove).
  function customFieldRowHTML(field, i) {
    return `
      <div class="cf-row" data-cf-index="${i}" style="padding:12px 14px;display:flex;flex-direction:column;gap:8px;">
        <div style="display:flex;align-items:center;gap:8px;">
          <input class="field-input" id="cf-title-${i}" maxlength="64" placeholder="Заголовок (например, «Часы работы»)"
            value="${esc(field.title)}" style="flex:1;">
          <button type="button" class="cf-remove" data-cf-remove="${i}" title="Удалить поле"
            style="flex-shrink:0;width:32px;height:32px;border:none;border-radius:8px;background:rgba(255,255,255,0.06);color:var(--danger);font-size:16px;cursor:pointer;">🗑</button>
        </div>
        <input class="field-input" id="cf-desc-${i}" placeholder="Зачем это поле — что ИИ должен учитывать"
          value="${esc(field.description)}">
        <textarea class="field-input field-textarea" id="cf-context-${i}" style="min-height:52px;"
          placeholder="Контекст (необязательно): значения, факты, материалы">${esc(field.context)}</textarea>
      </div>`;
  }

  function customFieldRowsHTML() {
    if (!customFields.length) {
      return `<div style="padding:14px;font-size:12px;color:var(--muted);">Пока нет дополнительных полей.</div>`;
    }
    return customFields.map((f, i) =>
      `<div style="border-bottom:1px solid var(--border,rgba(255,255,255,0.06));">${customFieldRowHTML(f, i)}</div>`
    ).join('');
  }

  function customFieldsHTML() {
    return `
      <div class="st-section">
        <p class="st-section-label">доп. поля роли</p>
        <div class="st-card" id="cf-list">${customFieldRowsHTML()}</div>
        <button type="button" class="btn btn-secondary" id="btn-add-cf" style="margin-top:8px;width:auto;padding:8px 14px;font-size:13px;">
          + добавить поле
        </button>
        <div style="font-size:11px;color:var(--muted);padding:6px 4px 0;line-height:1.4;">
          Свои поля для роли: заголовок и зачем оно обязательны, контекст — по желанию.
        </div>
      </div>`;
  }

  function roleDraftToolsHTML() {
    if (!isNew) return '';
    return `
      <div class="st-section">
        <p class="st-section-label">создать роль с AI</p>
        <div class="st-card" style="padding:14px;display:flex;flex-direction:column;gap:10px;">
          <textarea class="field-input field-textarea" id="role-draft-text" style="min-height:96px;"
            placeholder="Опиши роль свободно: кто она, как говорит, чего должна добиваться и чего делать нельзя."></textarea>
          <button type="button" class="btn btn-secondary" id="btn-draft-text">
            ✍️ Сгенерировать из текста
          </button>
          <button type="button" class="btn btn-secondary" id="btn-draft-voice">
            🎙 Описать голосом
          </button>
          <div id="role-draft-status" style="min-height:16px;font-size:11px;color:var(--muted);line-height:1.4;"></div>
        </div>
      </div>`;
  }

  let mediaRecorder = null;
  let audioStream = null;
  let audioChunks = [];
  let recordingTimeout = null;
  let screenDisposed = false;

  // Read the live input values back into customFields so add/remove/save never
  // lose what the user has typed.
  function collectCustomFields() {
    customFields = customFields.map((f, i) => ({
      title:       el.querySelector(`#cf-title-${i}`)?.value ?? f.title,
      description: el.querySelector(`#cf-desc-${i}`)?.value ?? f.description,
      context:     el.querySelector(`#cf-context-${i}`)?.value ?? f.context,
    }));
  }

  function renderCustomFieldRows() {
    const list = el.querySelector('#cf-list');
    if (!list) return;
    list.innerHTML = customFieldRowsHTML();
    list.querySelectorAll('[data-cf-remove]').forEach(btn => {
      btn.addEventListener('click', () => {
        twa?.HapticFeedback?.impactOccurred('light');
        collectCustomFields();
        customFields.splice(Number(btn.dataset.cfRemove), 1);
        renderCustomFieldRows();
      });
    });
  }

  function renderForm() {
    // Role is bound to the bot (Личный кабинет → Боты), so there is no per-chat
    // assignment here — only the role's own fields and (when editing) deletion.
    const deleteSection = isNew ? '' : `
      <div class="st-section">
        <div class="st-card">
          <div class="st-row st-row-danger" id="btn-delete-role">
            <span class="st-row-icon">🗑</span>
            <div class="st-row-body">
              <p class="st-row-title" style="color:var(--danger)">удалить роль</p>
              <p class="st-row-desc">бот вернётся к обычному ассистенту</p>
            </div>
          </div>
        </div>
      </div>`;

    el.querySelector('#re-scroll').innerHTML = `
      <div class="st-content">
        ${roleDraftToolsHTML()}
        <div class="st-section" style="display:flex;flex-direction:column;gap:16px;">
          ${fieldHTML('f-name', 'имя роли', 'Короткое название — как ты будешь её узнавать.', role.name,
            { required: true, placeholder: 'Менеджер, Техподдержка…' })}
          ${fieldHTML('f-desc', 'кто это', 'Главное поле: опиши персонажа — кто он и от чьего лица говорит.', role.description,
            { textarea: true, required: true, placeholder: 'Вежливый менеджер интернет-магазина, отвечает от имени Алексея…' })}
          ${fieldHTML('f-style', 'стиль речи', 'Тон, длина сообщений, лексика. Необязательно.', role.speaking_style,
            { textarea: true, placeholder: 'Коротко и по делу, без смайликов, на «вы»…' })}
          ${fieldHTML('f-goals', 'цели', 'Чего роль добивается в разговоре. Необязательно.', role.goals,
            { textarea: true, placeholder: 'Довести клиента до оформления заказа…' })}
          ${fieldHTML('f-constraints', 'ограничения', 'Что роли нельзя делать или обсуждать. Необязательно.', role.constraints,
            { textarea: true, placeholder: 'Не обещать скидки, не обсуждать конкурентов…' })}
        </div>

        ${speakAsSection()}
        ${autoLearnSection()}
        ${autoImproveSection()}
        ${modelSettingsHTML()}
        ${deliverySettingsHTML()}

        <div class="st-section">
          <p class="st-section-label">материалы роли (контекст)</p>
          <div class="field-group">
            <textarea class="field-input field-textarea" id="f-context" style="min-height:120px;"
              placeholder="Прайс, FAQ, описание услуг, важные факты — ИИ будет опираться на это в ответах.">${esc(role.context)}</textarea>
            <div style="display:flex;align-items:center;gap:8px;margin-top:8px;">
              <button type="button" class="btn btn-secondary" id="btn-attach" style="width:auto;padding:8px 14px;font-size:13px;">
                📎 Добавить файл
              </button>
              <span id="context-counter" style="font-size:11px;color:var(--muted);"></span>
            </div>
            <input type="file" id="file-input" accept=".txt,.md,.csv,.json,.text,text/*" multiple style="display:none;">
            <div style="font-size:11px;color:var(--muted);line-height:1.4;margin-top:6px;">
              Текстовые файлы (.txt, .md, .csv) — их содержимое добавится в материалы.
              Материалы хранятся в Telegram вместе с ролью.
            </div>
          </div>
        </div>
        ${customFieldsHTML()}
        ${deleteSection}
      </div>`;

    wireForm();
  }

  // _ROLE_CONTEXT_MAX mirrors the backend cap (roleContextMaxRunes) so the UI
  // refuses oversized material before the save round-trips.
  const ROLE_CONTEXT_MAX = 20000;

  function updateContextCounter() {
    const ta = el.querySelector('#f-context');
    const counter = el.querySelector('#context-counter');
    if (!ta || !counter) return;
    const len = ta.value.length;
    counter.textContent = len ? `${len} / ${ROLE_CONTEXT_MAX}` : '';
    counter.style.color = len > ROLE_CONTEXT_MAX ? 'var(--danger)' : 'var(--muted)';
  }

  function wireForm() {
    updateContextCounter();
    el.querySelector('#f-context')?.addEventListener('input', updateContextCounter);
    el.querySelector('#f-model-name')?.addEventListener('change', event => {
      role.model_name = event.target.value || '';
      role.model_tier = modelTierForName(role.model_name);
    });

    el.querySelector('#btn-draft-text')?.addEventListener('click', async () => {
      const text = el.querySelector('#role-draft-text')?.value.trim() || '';
      if (!text) {
        twa?.HapticFeedback?.notificationOccurred('error');
        twa?.showAlert('Сначала опиши роль текстом.');
        return;
      }
      await generateRoleDraft({ text });
    });

    el.querySelector('#btn-draft-voice')?.addEventListener('click', async () => {
      if (mediaRecorder?.state === 'recording') {
        mediaRecorder.stop();
        return;
      }
      await startRoleRecording();
    });

    // File attach: read text files in-browser and append their content to the
    // context. No upload endpoint — the text rides along in the role's context
    // field, persisted in Postgres (ai_roles.context_notes).
    const fileInput = el.querySelector('#file-input');
    el.querySelector('#btn-attach')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('light');
      fileInput.click();
    });
    fileInput?.addEventListener('change', async () => {
      const ta = el.querySelector('#f-context');
      for (const file of fileInput.files) {
        try {
          const text = await file.text();
          const block = `\n\n--- ${file.name} ---\n${text.trim()}`;
          ta.value = (ta.value + block).slice(0, ROLE_CONTEXT_MAX);
        } catch (_) {
          twa?.showAlert(`Не удалось прочитать файл «${file.name}».`);
        }
      }
      fileInput.value = '';
      updateContextCounter();
      twa?.HapticFeedback?.notificationOccurred('success');
    });

    // Custom fields: add a blank row / remove an existing one.
    renderCustomFieldRows();
    el.querySelector('#btn-add-cf')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('light');
      collectCustomFields();
      customFields.push({ title: '', description: '', context: '' });
      renderCustomFieldRows();
      el.querySelector(`#cf-title-${customFields.length - 1}`)?.focus();
    });

    el.querySelector('#btn-delete-role')?.addEventListener('click', () => {
      twa?.HapticFeedback?.impactOccurred('medium');
      twa?.showPopup({
        title: 'Удалить роль?',
        message: 'Бот вернётся к обычному ассистенту. Действие необратимо.',
        buttons: [
          { id: 'cancel', type: 'cancel', text: 'Отмена' },
          { id: 'delete', type: 'destructive', text: 'Удалить' },
        ],
      }, async (btnId) => {
        if (btnId !== 'delete') return;
        try {
          await api.deleteRole(roleId);
          twa?.HapticFeedback?.notificationOccurred('success');
          navigate('/roles');
        } catch (err) {
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    });
  }

  function setDraftBusy(busy, status = '') {
    const textBtn = el.querySelector('#btn-draft-text');
    const voiceBtn = el.querySelector('#btn-draft-voice');
    if (textBtn) textBtn.disabled = busy;
    if (voiceBtn) voiceBtn.disabled = busy;
    const statusEl = el.querySelector('#role-draft-status');
    if (statusEl) statusEl.textContent = status;
  }

  function applyRoleDraft(draft) {
    role = {
      ...role,
      name: draft.name || '',
      description: draft.description || '',
      speaking_style: draft.speaking_style || '',
      goals: draft.goals || '',
      constraints: draft.constraints || '',
      custom_fields: draft.custom_fields || [],
    };
    const fields = [
      ['#f-name', role.name],
      ['#f-desc', role.description],
      ['#f-style', role.speaking_style],
      ['#f-goals', role.goals],
      ['#f-constraints', role.constraints],
    ];
    fields.forEach(([selector, value]) => {
      const input = el.querySelector(selector);
      if (input) input.value = value;
    });
    customFields = role.custom_fields.map(field => ({
      title: field.title || '',
      description: field.description || '',
      context: field.context || '',
    }));
    renderCustomFieldRows();
    el.querySelector('#f-name')?.focus();
  }

  async function generateRoleDraft(payload) {
    setDraftBusy(true, 'AI собирает черновик роли…');
    try {
      const draft = await api.draftRole(payload);
      applyRoleDraft(draft);
      twa?.HapticFeedback?.notificationOccurred('success');
      setDraftBusy(false, 'Черновик готов — проверь поля и сохрани роль.');
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      setDraftBusy(false, 'Не удалось создать черновик.');
      twa?.showAlert(`Ошибка: ${err.message}`);
    }
  }

  async function startRoleRecording() {
    if (!navigator.mediaDevices?.getUserMedia || typeof MediaRecorder === 'undefined') {
      twa?.showAlert('Запись голоса не поддерживается в этом браузере.');
      return;
    }
    try {
      audioStream = await navigator.mediaDevices.getUserMedia({ audio: true });
      const preferred = 'audio/webm;codecs=opus';
      const options = MediaRecorder.isTypeSupported?.(preferred) ? { mimeType: preferred } : {};
      mediaRecorder = new MediaRecorder(audioStream, options);
      audioChunks = [];
      mediaRecorder.addEventListener('dataavailable', event => {
        if (event.data.size) audioChunks.push(event.data);
      });
      mediaRecorder.addEventListener('stop', async () => {
        if (recordingTimeout) clearTimeout(recordingTimeout);
        recordingTimeout = null;
        audioStream?.getTracks().forEach(track => track.stop());
        const mimeType = mediaRecorder.mimeType || audioChunks[0]?.type || 'audio/webm';
        const blob = new Blob(audioChunks, { type: mimeType });
        mediaRecorder = null;
        audioStream = null;
        audioChunks = [];
        if (screenDisposed || !blob.size) return;
        const voiceBtn = el.querySelector('#btn-draft-voice');
        if (voiceBtn) voiceBtn.textContent = '🎙 Описать голосом';
        setDraftBusy(true, 'Обрабатываю запись…');
        try {
          const audioBase64 = await blobToBase64(blob);
          await generateRoleDraft({ audio_base64: audioBase64, mime_type: mimeType });
        } catch (err) {
          setDraftBusy(false, 'Не удалось обработать запись.');
          twa?.HapticFeedback?.notificationOccurred('error');
          twa?.showAlert(err.message);
        }
      });
      mediaRecorder.start();
      const voiceBtn = el.querySelector('#btn-draft-voice');
      if (voiceBtn) voiceBtn.textContent = '⏹ Завершить запись';
      const textBtn = el.querySelector('#btn-draft-text');
      if (textBtn) textBtn.disabled = true;
      const status = el.querySelector('#role-draft-status');
      if (status) status.textContent = 'Идёт запись — нажми ещё раз, чтобы завершить.';
      recordingTimeout = setTimeout(() => {
        if (mediaRecorder?.state === 'recording') mediaRecorder.stop();
      }, 60000);
    } catch (err) {
      audioStream?.getTracks().forEach(track => track.stop());
      audioStream = null;
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Не удалось начать запись: ${err.message}`);
    }
  }

  function blobToBase64(blob) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(String(reader.result).split(',', 2)[1] || '');
      reader.onerror = () => reject(new Error('Не удалось прочитать аудиозапись.'));
      reader.readAsDataURL(blob);
    });
  }

  function collectRoleData() {
    collectCustomFields();
    const modelNameSelect = el.querySelector('#f-model-name');
    const selectedModel = modelNameSelect ? modelNameSelect.value : selectedModelName();
    // Keep only fields the user actually filled, and require title + why on each.
    const cleaned = customFields
      .map(f => ({ title: f.title.trim(), description: f.description.trim(), context: f.context.trim() }))
      .filter(f => f.title || f.description || f.context);
    return {
      name:           el.querySelector('#f-name')?.value.trim() || '',
      description:    el.querySelector('#f-desc')?.value.trim() || '',
      speaking_style: el.querySelector('#f-style')?.value.trim() || '',
      goals:          el.querySelector('#f-goals')?.value.trim() || '',
      constraints:    el.querySelector('#f-constraints')?.value.trim() || '',
      context:        el.querySelector('#f-context')?.value.trim() || '',
      custom_fields:  cleaned,
      rich_formatting_enabled: modeToNullableBool(el.querySelector('#f-rich-formatting')?.value),
      streaming_enabled:       modeToNullableBool(el.querySelector('#f-streaming')?.value),
      speak_as_owner:  el.querySelector('#f-speak-as-owner')?.checked ?? true,
      auto_learn_profile: el.querySelector('#f-auto-learn-profile')?.checked ?? true,
      auto_improve_skills: el.querySelector('#f-auto-improve-skills')?.checked ?? false,
      model_tier:      modelTierForName(selectedModel),
      model_name:      selectedModel,
    };
  }

  function validateRoleData(data, openingTools) {
    const badField = data.custom_fields.find(f => !f.title || !f.description);
    if (openingTools && (!data.name || !data.description || badField)) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert('сначала заполните имя и описание и сохраните роль');
      return false;
    }
    if (!data.name) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert('Укажи имя роли.');
      return false;
    }
    if (!data.description) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert('Опиши, кто эта роль — это главный материал для ИИ.');
      return false;
    }
    if (badField) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert('У каждого доп. поля должны быть заголовок и описание (зачем оно).');
      return false;
    }
    return true;
  }

  async function saveRole(openingTools) {
    const data = collectRoleData();
    if (!validateRoleData(data, openingTools)) return;
    const saveBtn = el.querySelector('#btn-save');
    const toolsBtn = el.querySelector('#btn-tools');
    const activeBtn = openingTools ? toolsBtn : saveBtn;
    activeBtn.textContent = '…';
    saveBtn.disabled = true;
    toolsBtn.disabled = true;
    try {
      let saved;
      if (isNew) {
        saved = await api.createRole(data);
      } else {
        saved = await api.updateRole(roleId, data);
      }
      twa?.HapticFeedback?.notificationOccurred('success');
      navigate(openingTools ? `/roles/${saved.id || roleId}/tools` : '/roles');
    } catch (err) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert(`Ошибка: ${err.message}`);
      saveBtn.textContent = isNew ? 'Создать роль' : 'Сохранить';
      toolsBtn.textContent = 'Инструменты';
      saveBtn.disabled = false;
      toolsBtn.disabled = false;
    }
  }

  el.querySelector('#btn-save').addEventListener('click', async () => {
    await saveRole(false);
  });

  el.querySelector('#btn-tools').addEventListener('click', async () => {
    if (isNew) {
      await saveRole(true);
      return;
    }
    navigate(`/roles/${roleId}/tools`);
  });

  async function load() {
    if (isNew) {
      // A template may carry starter custom fields; otherwise start empty.
      customFields = (role.custom_fields || []).map(f => ({
        title: f.title || '', description: f.description || '', context: f.context || '',
      }));
      await fetchModelsConfig();
      renderForm();
      return;
    }
    try {
      // Fetch role data and model config in parallel; config failure is silent.
      const [rolesData] = await Promise.all([api.roles(), fetchModelsConfig()]);
      const found = (rolesData.roles || []).find(r => String(r.id) === String(roleId));
      if (!found) {
        navigate('/roles');
        return;
      }
      role = found;
      customFields = (found.custom_fields || []).map(f => ({
        title: f.title || '', description: f.description || '', context: f.context || '',
      }));
      el.querySelector('#re-subtitle').textContent = role.name;
      renderForm();
    } catch (err) {
      el.querySelector('#re-scroll').innerHTML = `
        <div class="empty-state"><div class="empty-icon">⚠️</div><div class="empty-subtitle">${esc(err.message)}</div></div>`;
    }
  }

  load();
  return el;
}
