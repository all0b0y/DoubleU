import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

function esc(value) {
  return String(value || '').replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

/**
 * Owner screen to manage skills (Agent Skills-compatible content). Global
 * first-party skills (global=true) are read-only: shown but not editable or
 * deletable through the tenant API.
 */
export function renderSkills(navigate) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.innerHTML = `
    <div class="chat-topbar">
      <div class="chat-topbar-info">
        <div class="chat-topbar-name">навыки</div>
        <div class="chat-topbar-mode" style="color:var(--hint)">инструкции, которые роль подгружает по теме</div>
      </div>
    </div>
    <div class="scroll" id="sk-scroll">
      <div class="loading-row"><div class="spinner"></div></div>
    </div>
    <div style="padding:12px 16px calc(12px + var(--inset-bottom,0px));flex-shrink:0;">
      <button id="btn-sk-back" class="btn btn-secondary" style="width:100%">Назад</button>
    </div>`;

  const back = () => navigate('/settings');
  twa?.BackButton?.show();
  twa?.BackButton?.onClick(back);
  const observer = new MutationObserver(() => {
    if (!document.body.contains(el)) {
      twa?.BackButton?.offClick(back);
      observer.disconnect();
    }
  });
  observer.observe(document.body, { childList: true, subtree: true });
  el.querySelector('#btn-sk-back').addEventListener('click', back);

  function confirmAsync(msg) {
    return new Promise(resolve => {
      if (twa?.showConfirm) twa.showConfirm(msg, resolve);
      else resolve(window.confirm(msg));
    });
  }

  function skillCard(s) {
    const ro = s.global;
    return `
      <div class="st-card" style="margin-bottom:8px;padding:12px;display:flex;flex-direction:column;gap:6px;"
           data-skill="${s.id}">
        <input class="input" data-f="name" value="${esc(s.name)}" maxlength="64" ${ro ? 'disabled' : ''}>
        <input class="input" data-f="description" value="${esc(s.description)}" maxlength="500" ${ro ? 'disabled' : ''}>
        <textarea class="input" data-f="body" rows="4" maxlength="20000" ${ro ? 'disabled' : ''}
          placeholder="тело навыка (процедура)">${esc(s.body)}</textarea>
        ${ro
          ? `<p class="st-row-desc">встроенный навык — только для чтения</p>`
          : `<div style="display:flex;gap:8px;">
               <button class="btn btn-primary" data-save="${s.id}" style="flex:1">Сохранить</button>
               <button class="btn btn-secondary" data-del="${s.id}" style="flex:1">Удалить</button>
             </div>`}
      </div>`;
  }

  const addForm = `
    <div class="st-section">
      <p class="st-section-label">сгенерировать навык</p>
      <div class="st-card" style="padding:12px;display:flex;flex-direction:column;gap:8px;">
        <textarea id="sk-gen" class="input" rows="3" maxlength="8000"
          placeholder="Опишите навык своими словами (можно надиктовать голосом через микрофон клавиатуры) — ИИ оформит его."></textarea>
        <button id="sk-gen-btn" class="btn btn-primary">✨ Сгенерировать</button>
        <p class="st-row-desc">черновик подставится в поля ниже — проверьте и сохраните.</p>
      </div>
    </div>
    <div class="st-section">
      <p class="st-section-label">добавить навык</p>
      <div class="st-card" style="padding:12px;display:flex;flex-direction:column;gap:8px;">
        <input id="sk-name" class="input" placeholder="имя (до 64 символов)" maxlength="64">
        <input id="sk-desc" class="input" placeholder="когда применять (до 500)" maxlength="500">
        <textarea id="sk-body" class="input" rows="4" maxlength="20000" placeholder="тело навыка"></textarea>
        <button id="sk-add" class="btn btn-primary">Добавить</button>
      </div>
    </div>`;

  function fields(card) {
    return {
      name: card.querySelector('[data-f="name"]').value.trim(),
      description: card.querySelector('[data-f="description"]').value.trim(),
      body: card.querySelector('[data-f="body"]').value,
    };
  }

  function bind(root) {
    root.querySelectorAll('[data-save]').forEach(b => b.addEventListener('click', async () => {
      const card = root.querySelector(`[data-skill="${b.dataset.save}"]`);
      try {
        await api.updateSkill(b.dataset.save, fields(card));
        twa?.HapticFeedback?.notificationOccurred('success');
      } catch (err) { twa?.showAlert(`Ошибка: ${err.message}`); }
    }));
    root.querySelectorAll('[data-del]').forEach(b => b.addEventListener('click', async () => {
      if (!(await confirmAsync('Удалить навык?'))) return;
      try { await api.deleteSkill(b.dataset.del); load(); }
      catch (err) { twa?.showAlert(`Ошибка: ${err.message}`); }
    }));
  }

  async function load() {
    const scroll = el.querySelector('#sk-scroll');
    try {
      const data = await api.skills();
      const skills = data.skills || [];
      const list = skills.length
        ? skills.map(skillCard).join('')
        : `<p class="st-row-desc" style="padding:8px 4px;">навыков пока нет</p>`;
      scroll.innerHTML = `<div class="st-content">
        <div class="st-section"><p class="st-section-label">навыки</p>${list}</div>
        ${addForm}
      </div>`;
      bind(scroll);
      scroll.querySelector('#sk-gen-btn').addEventListener('click', async () => {
        const genBtn = scroll.querySelector('#sk-gen-btn');
        const text = scroll.querySelector('#sk-gen').value.trim();
        if (!text) { twa?.showAlert('Опишите навык, который нужно создать.'); return; }
        genBtn.disabled = true;
        genBtn.textContent = '✨ Генерирую…';
        try {
          const draft = await api.draftSkill({ text });
          scroll.querySelector('#sk-name').value = draft.name || '';
          scroll.querySelector('#sk-desc').value = draft.description || '';
          scroll.querySelector('#sk-body').value = draft.body || '';
          twa?.HapticFeedback?.notificationOccurred('success');
        } catch (err) {
          twa?.HapticFeedback?.notificationOccurred('error');
          twa?.showAlert(`Ошибка: ${err.message}`);
        } finally {
          genBtn.disabled = false;
          genBtn.textContent = '✨ Сгенерировать';
        }
      });
      scroll.querySelector('#sk-add').addEventListener('click', async () => {
        const body = {
          name: scroll.querySelector('#sk-name').value.trim(),
          description: scroll.querySelector('#sk-desc').value.trim(),
          body: scroll.querySelector('#sk-body').value,
        };
        try {
          await api.createSkill(body);
          twa?.HapticFeedback?.notificationOccurred('success');
          load();
        } catch (err) {
          twa?.HapticFeedback?.notificationOccurred('error');
          twa?.showAlert(`Ошибка: ${err.message}`);
        }
      });
    } catch (err) {
      scroll.innerHTML = `<div class="empty-state"><div class="empty-icon">⚠️</div>
        <div class="empty-subtitle">${esc(err.message)}</div></div>`;
    }
  }

  load();
  return el;
}
