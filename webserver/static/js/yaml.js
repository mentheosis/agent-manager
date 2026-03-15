// Minimal YAML parser for orchestrator team config.
// Supports: scalars, multiline | blocks, sequences of mappings (- key: val).
// Does NOT handle anchors, aliases, flow style, or complex nesting.

export function parseYAML(text) {
  const lines = text.split('\n');
  const result = {};
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Skip blank lines and comments
    if (line.trim() === '' || line.trim().startsWith('#')) {
      i++;
      continue;
    }

    const match = line.match(/^(\w[\w_]*):\s*(.*)/);
    if (!match) {
      i++;
      continue;
    }

    const key = match[1];
    let value = match[2].trim();

    if (value === '|' || value === '>') {
      // Multiline block scalar
      const fold = value === '>';
      i++;
      const blockLines = [];
      const indent = detectIndent(lines, i);
      while (i < lines.length) {
        const bl = lines[i];
        if (bl.trim() === '') {
          blockLines.push('');
          i++;
          continue;
        }
        if (getIndent(bl) < indent && bl.trim() !== '') break;
        blockLines.push(bl.slice(indent));
        i++;
      }
      // Trim trailing blank lines
      while (blockLines.length > 0 && blockLines[blockLines.length - 1] === '') {
        blockLines.pop();
      }
      result[key] = fold ? blockLines.join(' ') : blockLines.join('\n');
    } else if (value === '' || value === '[]') {
      // Check if next line starts a sequence
      i++;
      if (value === '[]') {
        result[key] = [];
        continue;
      }
      if (i < lines.length && lines[i].trim().startsWith('-')) {
        result[key] = parseSequence(lines, i);
        // Advance past the sequence
        i = skipSequence(lines, i);
      } else {
        result[key] = '';
      }
    } else {
      // Simple scalar
      result[key] = parseScalar(value);
      i++;
    }
  }

  return result;
}

function parseScalar(s) {
  // Remove quotes
  if ((s.startsWith('"') && s.endsWith('"')) || (s.startsWith("'") && s.endsWith("'"))) {
    return s.slice(1, -1);
  }
  if (s === 'true') return true;
  if (s === 'false') return false;
  if (s === 'null') return null;
  if (/^-?\d+$/.test(s)) return parseInt(s, 10);
  if (/^-?\d+\.\d+$/.test(s)) return parseFloat(s);
  return s;
}

function getIndent(line) {
  const m = line.match(/^(\s*)/);
  return m ? m[1].length : 0;
}

function detectIndent(lines, start) {
  for (let i = start; i < lines.length; i++) {
    if (lines[i].trim() !== '') return getIndent(lines[i]);
  }
  return 2;
}

function parseSequence(lines, start) {
  const items = [];
  let i = start;
  const seqIndent = getIndent(lines[i]);

  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === '' || line.trim().startsWith('#')) {
      i++;
      continue;
    }
    if (getIndent(line) < seqIndent && line.trim() !== '') break;
    if (!line.trim().startsWith('-')) break;

    // Start of a new item
    const afterDash = line.trim().slice(1).trim();
    const item = {};

    if (afterDash.includes(':')) {
      // Inline mapping on same line as dash: "- key: value"
      const kv = afterDash.match(/^(\w[\w_]*):\s*(.*)/);
      if (kv) {
        item[kv[1]] = parseScalar(kv[2].trim());
      }
      i++;

      // Continuation keys at deeper indent
      const itemIndent = getIndent(line) + 2;
      while (i < lines.length) {
        const cl = lines[i];
        if (cl.trim() === '' || cl.trim().startsWith('#')) {
          i++;
          continue;
        }
        if (getIndent(cl) < itemIndent) break;
        if (cl.trim().startsWith('-')) break;
        const ckv = cl.trim().match(/^(\w[\w_]*):\s*(.*)/);
        if (ckv) {
          let val = ckv[2].trim();
          if (val === '|' || val === '>') {
            const fold = val === '>';
            i++;
            const blockLines = [];
            const blockIndent = detectIndent(lines, i);
            while (i < lines.length) {
              const bl = lines[i];
              if (bl.trim() === '') {
                blockLines.push('');
                i++;
                continue;
              }
              if (getIndent(bl) < blockIndent && bl.trim() !== '') break;
              blockLines.push(bl.slice(blockIndent));
              i++;
            }
            while (blockLines.length > 0 && blockLines[blockLines.length - 1] === '') {
              blockLines.pop();
            }
            item[ckv[1]] = fold ? blockLines.join(' ') : blockLines.join('\n');
          } else {
            item[ckv[1]] = parseScalar(val);
            i++;
          }
        } else {
          i++;
        }
      }
    } else if (afterDash !== '') {
      // Simple scalar item
      items.push(parseScalar(afterDash));
      i++;
      continue;
    } else {
      i++;
      // Mapping on subsequent lines
      const itemIndent = detectIndent(lines, i);
      while (i < lines.length) {
        const cl = lines[i];
        if (cl.trim() === '' || cl.trim().startsWith('#')) {
          i++;
          continue;
        }
        if (getIndent(cl) < itemIndent) break;
        if (cl.trim().startsWith('-')) break;
        const ckv = cl.trim().match(/^(\w[\w_]*):\s*(.*)/);
        if (ckv) {
          item[ckv[1]] = parseScalar(ckv[2].trim());
          i++;
        } else {
          i++;
        }
      }
    }

    if (Object.keys(item).length > 0) {
      items.push(item);
    }
  }

  return items;
}

function skipSequence(lines, start) {
  let i = start;
  const seqIndent = getIndent(lines[i]);

  while (i < lines.length) {
    const line = lines[i];
    if (line.trim() === '' || line.trim().startsWith('#')) {
      i++;
      continue;
    }
    if (getIndent(line) < seqIndent && line.trim() !== '') break;
    if (!line.trim().startsWith('-') && getIndent(line) <= seqIndent) break;
    i++;
  }
  return i;
}
