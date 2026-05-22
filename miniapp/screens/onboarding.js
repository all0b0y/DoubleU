import { api } from '/miniapp/api/client.js';

const twa = window.Telegram?.WebApp;

export function renderOnboarding(navigate, reason) {
  const el = document.createElement('div');
  el.className = 'screen';
  el.style.cssText = 'height:100%;display:flex;flex-direction:column;overflow:hidden;';

  el.innerHTML = `
    <div style="flex:1;display:flex;flex-direction:column;padding:20px 20px 16px;overflow:hidden;">

      <!-- Top row: back + step counter -->
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:20px;flex-shrink:0;">
        <button id="btn-back" style="background:transparent;border:none;padding:0;cursor:pointer;display:flex;align-items:center;gap:4px;color:var(--hint);font-size:13px;">
          ‹ назад
        </button>
        <span style="font-size:12px;color:var(--hint);">шаг 1 из 2</span>
      </div>

      <!-- Progress bar -->
      <div style="display:flex;gap:6px;margin-bottom:24px;flex-shrink:0;">
        <div style="flex:1;height:3px;background:var(--accent);border-radius:2px;"></div>
        <div style="flex:1;height:3px;background:var(--bg2);border-radius:2px;"></div>
      </div>

      <!-- Title + subtitle -->
      <h2 style="font-size:20px;font-weight:500;color:var(--text);margin:0 0 6px;letter-spacing:-0.3px;flex-shrink:0;">
        Откройте настройки Telegram
      </h2>
      <p style="font-size:13px;color:var(--hint);margin:0 0 18px;line-height:1.5;flex-shrink:0;">
        ${reason === 'revoked'
          ? '⚠️ Доступ был отозван. Снова найдите раздел «Devices» и переподключите DoubleU'
          : 'найдите раздел «Devices» и подтвердите подключение DoubleU'
        }
      </p>

      <!-- GIF placeholder — fills remaining space -->
      <div id="gif-area" style="
        flex:1;
        background:var(--bg2);
        border:1.5px dashed var(--accent);
        border-radius:16px;
        display:flex;flex-direction:column;align-items:center;justify-content:center;gap:8px;
        min-height:0;
        overflow:hidden;
        margin-bottom:16px;
      ">
        <svg width="36" height="36" viewBox="0 0 24 24" fill="none" stroke="#6366F1" stroke-width="1.5"><rect x="3" y="3" width="18" height="18" rx="3"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="M21 15l-5-5L5 21"/></svg>
        <span style="font-size:11px;color:var(--accent);">место под гифку</span>
      </div>

      <!-- Waiting status bar -->
      <div id="status-bar" style="
        display:flex;align-items:center;gap:10px;
        padding:12px 14px;
        background:var(--bg2);
        border-radius:12px;
        flex-shrink:0;
      ">
        <div class="spinner" style="width:16px;height:16px;border-width:2px;flex-shrink:0;"></div>
        <span style="font-size:13px;color:var(--text);">ждём подключения…</span>
        <button id="btn-check" style="
          margin-left:auto;background:none;border:none;
          color:var(--accent);font-size:12px;cursor:pointer;padding:0;flex-shrink:0;
        ">проверить</button>
      </div>

    </div>
  `;

  el.querySelector('#btn-back').addEventListener('click', () => {
    twa?.HapticFeedback?.impactOccurred('light');
    navigate('/');
  });

  let pollTimer = null;

  function showStep2() {
    clearInterval(pollTimer);
    twa?.HapticFeedback?.notificationOccurred('success');
    showSuccessOverlay(() => navigate('/chats'));
  }

  async function checkConnection() {
    try {
      const me = await api.me();
      if (me.connection.status === 'connected') { showStep2(); return true; }
    } catch (_) {}
    return false;
  }

  el.querySelector('#btn-check').addEventListener('click', async () => {
    twa?.HapticFeedback?.impactOccurred('light');
    const ok = await checkConnection();
    if (!ok) {
      twa?.HapticFeedback?.notificationOccurred('error');
      twa?.showAlert('Подключение не найдено. Убедись что выбрал @DoubleUBot в Telegram Business → Devices.');
    }
  });

  pollTimer = setInterval(checkConnection, 4000);
  checkConnection();

  const obs = new MutationObserver(() => {
    if (!document.body.contains(el)) { clearInterval(pollTimer); obs.disconnect(); }
  });
  obs.observe(document.body, { childList: true, subtree: true });

  return el;
}

// Step 2 — fullscreen success overlay with confetti, auto-dismisses after 2.5s
function showSuccessOverlay(onDone) {
  const overlay = document.createElement('div');
  overlay.style.cssText = `
    position:fixed;inset:0;z-index:200;
    background:var(--bg);
    display:flex;flex-direction:column;align-items:center;justify-content:center;gap:20px;
    animation:fadeIn 180ms ease;
  `;

  // Confetti
  const colors = ['#6366F1','#10B981','#F59E0B','#EC4899','#3B82F6','#8B5CF6'];
  const dots = Array.from({ length: 36 }, (_, i) => {
    const c = colors[i % colors.length];
    const x = (Math.random() * 240 - 120).toFixed(0);
    const rot = (Math.random() * 720 - 360).toFixed(0);
    const delay = (Math.random() * 0.5).toFixed(2);
    const dur = (0.9 + Math.random() * 0.7).toFixed(2);
    const left = (10 + Math.random() * 80).toFixed(0);
    const size = 6 + Math.floor(Math.random() * 6);
    return `<div style="
      position:absolute;left:${left}%;top:-10px;
      width:${size}px;height:${size}px;border-radius:${Math.random() > 0.5 ? '50%' : '2px'};
      background:${c};
      animation:confettiFall ${dur}s ${delay}s ease-in both;
      --tx:${x}px;--rot:${rot}deg;
    "></div>`;
  }).join('');

  overlay.innerHTML = `
    <style>
      @keyframes fadeIn { from{opacity:0} to{opacity:1} }
      @keyframes confettiFall {
        0%   { transform:translate(0,-20px) rotate(0deg); opacity:1; }
        100% { transform:translate(var(--tx),100vh) rotate(var(--rot)); opacity:0; }
      }
      @keyframes popIn { from{transform:scale(0.3);opacity:0} to{transform:scale(1);opacity:1} }
      @keyframes checkBounce { from{transform:scale(0.5) rotate(-15deg);opacity:0} to{transform:none;opacity:1} }
      @keyframes fadeUp { from{transform:translateY(12px);opacity:0} to{transform:none;opacity:1} }
    </style>
    <div style="position:absolute;inset:0;pointer-events:none;overflow:hidden;">${dots}</div>

    <div style="
      width:96px;height:96px;border-radius:50%;
      background:rgba(16,185,129,0.12);
      display:flex;align-items:center;justify-content:center;
      animation:popIn 380ms cubic-bezier(0.34,1.56,0.64,1);
    ">
      <span style="font-size:48px;animation:checkBounce 320ms 120ms ease both;display:block;">✅</span>
    </div>

    <div style="text-align:center;animation:fadeUp 300ms 180ms ease both;">
      <div style="font-size:22px;font-weight:700;color:var(--text);margin-bottom:6px;">Подключено!</div>
      <div style="font-size:14px;color:var(--hint);">DoubleU готов к работе</div>
    </div>
  `;

  document.getElementById('app').appendChild(overlay);
  setTimeout(() => { overlay.remove(); onDone(); }, 2500);
}
