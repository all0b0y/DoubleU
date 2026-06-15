// Brand glyphs — replace system emoji with the DoubleU hand-drawn set, and
// render work-chat avatars as cat faces instead of initials.
//
// Iteration 2 of the design handoff: the user asked to swap every system emoji
// for the matcha/cream hand-drawn glyphs, and to use the sunglasses-cat family
// for chat avatars instead of "АК" initials (see the design system bundle).
//
// The SVGs/PNGs are self-coloured, so they render as plain <img>. We map at the
// DOM level (brandify) rather than editing every template string: one place,
// covers all screens — including content rendered asynchronously after load.

const BASE = '/miniapp/assets';

// system emoji → brand asset (relative to assets/). Unmapped emoji are left
// untouched, so functional Unicode marks (arrows, chevrons) keep working.
const MAP = {
  '💬': 'icons/message.png',
  '🎭': 'icons/sticker-smile.png',
  '🗑': 'icons/trash.svg',
  '✨': 'icons/sparkles.png',
  '🔄': 'icons/refresh.svg',
  '🤖': 'cats/cat-glasses.svg',   // mascot, not a robot cliché
  '⚡': 'icons/flame.png',
  '🧠': 'icons/layers.png',
  '🤔': 'cats/cat-content.svg',
  '💭': 'icons/message.png',
  '🪄': 'icons/wand.png',
  '✏': 'icons/edit.svg',
  '🔒': 'icons/chat-lock.png',
  '✅': 'emoji/check.svg',
  '✔': 'emoji/check.svg',
  '✖': 'icons/close.svg',
  '✕': 'icons/close.svg',
  '✗': 'icons/close.svg',
  '👍': 'emoji/thumbsup.svg',
  '👎': 'emoji/sad.svg',
  '🔍': 'icons/target.png',
  '📊': 'icons/venn.png',
  '💼': 'icons/briefcase.png',
  '🛟': 'icons/lifebuoy.svg',
  '💡': 'emoji/bulb.svg',
  '👥': 'icons/group.png',
  '👤': 'icons/person-card.png',
  '🎓': 'icons/easel.png',
  '📚': 'icons/layers.png',
  '⚙': 'icons/gear.svg',
  '🔌': 'cats/cat-phones.svg',
  '⚠': 'emoji/sad.svg',
  '🎤': 'icons/mic.svg',
  '🎵': 'icons/mic.svg',
  '📎': 'icons/tag.png',
  '📷': 'icons/easel.png',
  '🖼': 'icons/easel.png',
  '🎥': 'icons/easel.png',
  '📹': 'icons/easel.png',
  '🎬': 'icons/easel.png',
  '💾': 'icons/check.svg',
  '↑': 'icons/send.svg',
};

// Alternation of literal emoji; matched globally over text nodes.
const EMOJI_RE = new RegExp('(' + Object.keys(MAP).join('|') + ')', 'g');

function imgFor(emoji) {
  return `<img class="glyph" src="${BASE}/${MAP[emoji]}" alt="" aria-hidden="true">`;
}

/**
 * Replace mapped emoji inside a DOM subtree with brand <img> glyphs.
 * Walks text nodes only (never attributes), skipping inputs and already-swapped
 * nodes, so it is idempotent and safe to run repeatedly.
 * @param {Node} root
 */
export function brandify(root) {
  if (!root) return;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode(node) {
      const p = node.parentNode;
      if (!p) return NodeFilter.FILTER_REJECT;
      const tag = p.nodeName;
      if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'TEXTAREA') return NodeFilter.FILTER_REJECT;
      if (p.classList && p.classList.contains('glyph-text')) return NodeFilter.FILTER_REJECT;
      EMOJI_RE.lastIndex = 0;
      return EMOJI_RE.test(node.nodeValue) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT;
    },
  });

  const targets = [];
  for (let n = walker.nextNode(); n; n = walker.nextNode()) targets.push(n);

  for (const node of targets) {
    EMOJI_RE.lastIndex = 0;
    const html = node.nodeValue.replace(EMOJI_RE, (m) => imgFor(m));
    const span = document.createElement('span');
    span.className = 'glyph-text';
    span.innerHTML = html;
    node.parentNode.replaceChild(span, node);
  }
}

// ── Cat avatars ──────────────────────────────────────────────────────────────

const CATS = [
  'cat-glasses', 'cat-content', 'cat-love', 'cat-surprise', 'cat-sleepy',
  'cat-happy', 'cat-calm', 'cat-wink', 'cat-specs', 'cat-phones',
];

/** Deterministic cat pick for a stable seed (chat id / name) so an avatar never changes. */
export function catFor(seed) {
  let h = 0;
  for (const ch of String(seed || '?')) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return CATS[h % CATS.length];
}

/** Inner <img> for a cat avatar, to drop inside an existing `.avatar`/`.chat-av` element. */
export function catImg(seed) {
  return `<img class="cat-face" src="${BASE}/cats/${catFor(seed)}.svg" alt="" aria-hidden="true">`;
}
