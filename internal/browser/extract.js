/* __remote-chrome_extract__ — readability-lite article extraction.
 *
 * Runs in the page via Runtime.evaluate (returnByValue). Finds the main
 * content container readability-style (score the parents of substantial
 * <p>/<pre>/<blockquote> blocks, penalize link-dense containers like menus),
 * then emits it as Markdown so headings, links, lists and tables survive.
 *
 * Returns {title, byline, markdown} or null when no main content is
 * detected (the Go side then falls back to innerText).
 */
(() => {
  if (!document.body) return null;

  const NOISE_SEL = [
    'nav', 'header', 'footer', 'aside', 'script', 'style', 'noscript',
    'form', 'iframe', 'svg', 'button',
    '[role="navigation"]', '[role="banner"]', '[role="contentinfo"]',
    '[role="complementary"]', '[role="dialog"]', '[role="search"]',
    '[aria-hidden="true"]',
  ].join(',');

  const isNoise = (el) => el.nodeType === 1 && el.matches(NOISE_SEL);

  const linkDensity = (el) => {
    const total = (el.innerText || '').length || 1;
    let linked = 0;
    for (const a of el.querySelectorAll('a')) linked += (a.innerText || '').length;
    return linked / total;
  };

  // ---- candidate selection: score the parents of real text blocks ----
  const scores = new Map();
  for (const p of document.querySelectorAll('p,pre,blockquote')) {
    if (p.closest(NOISE_SEL)) continue;
    const len = (p.innerText || '').trim().length;
    if (len < 25) continue;
    const parent = p.parentElement;
    if (!parent) continue;
    scores.set(parent, (scores.get(parent) || 0) + len);
    const gp = parent.parentElement;
    if (gp) scores.set(gp, (scores.get(gp) || 0) + len / 2);
  }
  let best = null;
  let bestScore = 0;
  for (const [el, s] of scores) {
    const adj = s * (1 - Math.min(linkDensity(el), 0.9));
    if (adj > bestScore) { best = el; bestScore = adj; }
  }
  const semantic = document.querySelector('article, main, [role="main"]');
  if (best && semantic && best.contains(semantic)) best = semantic; // best too broad
  if (!best && semantic) { best = semantic; bestScore = (semantic.innerText || '').trim().length; }
  if (!best || bestScore < 200) return null;

  // ---- markdown emitter ----
  const abs = (u) => { try { return new URL(u, location.href).href; } catch { return u; } };
  const collapse = (s) => s.replace(/\s+/g, ' ');

  function inline(node) {
    let out = '';
    for (const n of node.childNodes) {
      if (n.nodeType === 3) { out += collapse(n.textContent); continue; }
      if (n.nodeType !== 1 || isNoise(n)) continue;
      const tag = n.tagName.toLowerCase();
      if (tag === 'ul' || tag === 'ol') continue; // nested lists render as blocks
      const inner = inline(n);
      if (tag === 'a' && n.getAttribute('href') && inner.trim()) {
        out += '[' + inner.trim() + '](' + abs(n.getAttribute('href')) + ')';
      } else if ((tag === 'strong' || tag === 'b') && inner.trim()) {
        out += '**' + inner.trim() + '**';
      } else if ((tag === 'em' || tag === 'i') && inner.trim()) {
        out += '*' + inner.trim() + '*';
      } else if (tag === 'code') {
        out += '`' + n.textContent + '`';
      } else if (tag === 'br') {
        out += '\n';
      } else if (tag === 'img') {
        if (n.alt) out += '![' + n.alt + '](' + abs(n.getAttribute('src') || '') + ')';
      } else {
        out += inner;
      }
    }
    return out;
  }

  function listItems(el, ordered, indent) {
    let out = '';
    let i = 1;
    for (const li of el.children) {
      if (li.tagName.toLowerCase() !== 'li') continue;
      out += indent + (ordered ? (i++) + '. ' : '- ') + inline(li).trim() + '\n';
      for (const sub of li.children) {
        const t = sub.tagName.toLowerCase();
        if (t === 'ul' || t === 'ol') out += listItems(sub, t === 'ol', indent + '  ');
      }
    }
    return out;
  }

  function table(el) {
    const rows = [...el.querySelectorAll('tr')].slice(0, 30);
    if (!rows.length) return '';
    let out = '\n';
    rows.forEach((tr, ri) => {
      const cells = [...tr.children].map((c) => inline(c).trim().replace(/\|/g, '\\|'));
      out += '| ' + cells.join(' | ') + ' |\n';
      if (ri === 0) out += '|' + cells.map(() => ' --- ').join('|') + '|\n';
    });
    return out + '\n';
  }

  const BLOCK = new Set(['div', 'section', 'article', 'main', 'figure', 'span', 'dl', 'dd', 'dt']);

  function render(el) {
    let out = '';
    for (const n of el.childNodes) {
      if (n.nodeType === 3) {
        const t = collapse(n.textContent);
        if (t.trim()) out += t;
        continue;
      }
      if (n.nodeType !== 1 || isNoise(n)) continue;
      const tag = n.tagName.toLowerCase();
      if (/^h[1-6]$/.test(tag)) {
        const t = inline(n).trim();
        if (t) out += '\n' + '#'.repeat(+tag[1]) + ' ' + t + '\n\n';
      } else if (tag === 'p') {
        const t = inline(n).trim();
        if (t) out += t + '\n\n';
      } else if (tag === 'ul' || tag === 'ol') {
        out += '\n' + listItems(n, tag === 'ol', '') + '\n';
      } else if (tag === 'blockquote') {
        const t = inline(n).trim();
        if (t) out += '\n> ' + t.replace(/\n/g, '\n> ') + '\n\n';
      } else if (tag === 'pre') {
        out += '\n```\n' + n.textContent.replace(/\n$/, '') + '\n```\n\n';
      } else if (tag === 'table') {
        out += table(n);
      } else if (tag === 'hr') {
        out += '\n---\n\n';
      } else if (tag === 'img') {
        if (n.alt) out += '![' + n.alt + '](' + abs(n.getAttribute('src') || '') + ')\n\n';
      } else if (BLOCK.has(tag)) {
        out += render(n);
      } else {
        const t = inline(n).trim();
        if (t) out += t + '\n\n';
      }
    }
    return out;
  }

  const bylineEl = document.querySelector('[rel="author"], .byline, [itemprop="author"]');
  const byline = (document.querySelector('meta[name="author"]') || {}).content ||
    (bylineEl ? bylineEl.innerText : '').trim();

  let md = render(best).replace(/\n{3,}/g, '\n\n').trim();
  if (md.length > 40000) md = md.slice(0, 40000) + '\n\n…[truncated]';
  if (md.length < 200) return null;
  return { title: document.title, byline: byline || '', markdown: md };
})()
