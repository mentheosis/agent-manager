// ANSI-to-HTML parser
const ANSI_COLORS_16 = [
  '#1a1b26','#f7768e','#9ece6a','#e0af68','#7aa2f7','#bb9af7','#7dcfff','#c0caf5',
  '#565f89','#f7768e','#9ece6a','#e0af68','#7aa2f7','#bb9af7','#7dcfff','#ffffff',
];

// 256-color palette: 16 base + 216 cube + 24 grayscale
const ANSI_256 = (() => {
  const c = [...ANSI_COLORS_16];
  for (let r = 0; r < 6; r++)
    for (let g = 0; g < 6; g++)
      for (let b = 0; b < 6; b++)
        c.push(`#${[r,g,b].map(v => (v ? v*40+55 : 0).toString(16).padStart(2,'0')).join('')}`);
  for (let i = 0; i < 24; i++) {
    const v = (i * 10 + 8).toString(16).padStart(2, '0');
    c.push(`#${v}${v}${v}`);
  }
  return c;
})();

export function esc(s) {
  return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function openSpan(fg, bg, bold, dim, italic, underline, inverse, strikethrough) {
  const styles = [];
  const classes = [];
  const fgActual = inverse ? bg : fg;
  const bgActual = inverse ? (fg || 'var(--text)') : bg;
  if (fgActual) styles.push(`color:${fgActual}`);
  if (bgActual) styles.push(`background:${bgActual}`);
  if (bold) classes.push('ansi-bold');
  if (dim) classes.push('ansi-dim');
  if (italic) classes.push('ansi-italic');
  if (underline) classes.push('ansi-underline');
  if (strikethrough) classes.push('ansi-strikethrough');
  let tag = '<span';
  if (classes.length) tag += ` class="${classes.join(' ')}"`;
  if (styles.length) tag += ` style="${styles.join(';')}"`;
  return tag + '>';
}

export function ansiToHtml(text) {
  if (!text) return '';
  const re = /\x1b\[[0-9;]*m|\x1b\[[0-9;]*[A-HJKSTfhln]|\x1b\][\s\S]*?(?:\x07|\x1b\\)|\x1b[^[\]]/g;

  let fg = null, bg = null;
  let bold = false, dim = false, italic = false, underline = false, inverse = false, strikethrough = false;
  let result = '';
  let lastIdx = 0;

  for (const match of text.matchAll(re)) {
    if (match.index > lastIdx) {
      result += openSpan(fg, bg, bold, dim, italic, underline, inverse, strikethrough);
      result += esc(text.slice(lastIdx, match.index));
      result += '</span>';
    }
    lastIdx = match.index + match[0].length;

    const seq = match[0];
    if (!seq.endsWith('m')) continue;

    const codes = seq.slice(2, -1).split(';').map(s => s === '' ? 0 : parseInt(s, 10));

    for (let i = 0; i < codes.length; i++) {
      const c = codes[i];
      if (c === 0) { fg = bg = null; bold = dim = italic = underline = inverse = strikethrough = false; }
      else if (c === 1) bold = true;
      else if (c === 2) dim = true;
      else if (c === 3) italic = true;
      else if (c === 4) underline = true;
      else if (c === 7) inverse = true;
      else if (c === 9) strikethrough = true;
      else if (c === 22) { bold = false; dim = false; }
      else if (c === 23) italic = false;
      else if (c === 24) underline = false;
      else if (c === 27) inverse = false;
      else if (c === 29) strikethrough = false;
      else if (c >= 30 && c <= 37) fg = ANSI_COLORS_16[c - 30];
      else if (c === 38) {
        if (codes[i+1] === 5 && codes[i+2] != null) { fg = ANSI_256[codes[i+2]]; i += 2; }
        else if (codes[i+1] === 2 && codes[i+4] != null) {
          fg = `rgb(${codes[i+2]},${codes[i+3]},${codes[i+4]})`; i += 4;
        }
      }
      else if (c === 39) fg = null;
      else if (c >= 40 && c <= 47) bg = ANSI_COLORS_16[c - 40];
      else if (c === 48) {
        if (codes[i+1] === 5 && codes[i+2] != null) { bg = ANSI_256[codes[i+2]]; i += 2; }
        else if (codes[i+1] === 2 && codes[i+4] != null) {
          bg = `rgb(${codes[i+2]},${codes[i+3]},${codes[i+4]})`; i += 4;
        }
      }
      else if (c === 49) bg = null;
      else if (c >= 90 && c <= 97) fg = ANSI_COLORS_16[c - 90 + 8];
      else if (c >= 100 && c <= 107) bg = ANSI_COLORS_16[c - 100 + 8];
    }
  }

  if (lastIdx < text.length) {
    result += openSpan(fg, bg, bold, dim, italic, underline, inverse, strikethrough);
    result += esc(text.slice(lastIdx));
    result += '</span>';
  }

  return result;
}

export function safeAnsiToHtml(text) {
  const tmp = document.createElement('div');
  tmp.innerHTML = ansiToHtml(text);
  return tmp.innerHTML;
}
