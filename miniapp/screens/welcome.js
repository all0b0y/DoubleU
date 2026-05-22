export function renderWelcome(navigate) {
  const el = document.createElement('div');
  el.className = 'screen welcome-screen';
  el.innerHTML = `
    <div class="welcome-logo">🤖</div>
    <div>
      <div class="welcome-title">DoubleU</div>
      <div class="welcome-subtitle" style="margin-top:8px">
        AI-ассистент для Telegram Business.<br>Отвечает на сообщения вместо тебя.
      </div>
    </div>
    <div style="width:100%;display:flex;flex-direction:column;gap:10px;margin-top:12px">
      <button class="btn btn-primary" id="btn-start">Начать →</button>
    </div>
  `;
  el.querySelector('#btn-start').addEventListener('click', () => {
    window.Telegram?.WebApp?.HapticFeedback?.impactOccurred('medium');
    navigate('/onboarding');
  });
  return el;
}
