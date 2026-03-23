import { safeAnsiToHtml, esc } from './ansi.js';
import { api } from './api.js';

// ConvLogView manages the terminal output display for all instances.
// It keeps a per-instance cache of DOM nodes and WebSocket connections
// so that background conversations continue receiving updates and
// switching conversations is instant (no re-fetch).
export class ConvLogView {
  constructor(container) {
    this.container = container;
    this.currentTitle = null;
    this.onStatus = null; // callback(title, status) for sidebar updates
    this.onContent = null; // callback(content) for mode detection etc.
    // Per-instance state: title -> { wrapper, historyDiv, paneDiv, lastInputDiv, stableCount, ws, gen }
    this._cache = {};
    this._genCounter = 0;
  }

  // Connect to (display) an instance. Keeps previous instance's WS alive in background.
  async connect(title) {
    this.currentTitle = title;

    let entry = this._cache[title];
    if (entry) {
      // Reattach cached DOM
      this.container.innerHTML = '';
      this.container.appendChild(entry.wrapper);

      // Fire onContent for action button detection
      if (this.onContent && entry.paneDiv.textContent) {
        this.onContent(entry.paneDiv.textContent);
      }

      this.container.scrollTop = this.container.scrollHeight;

      // If WS died while in background, reconnect it
      if (!entry.ws || entry.ws.readyState > WebSocket.OPEN) {
        this._connectWs(title, entry);
      }
      return;
    }

    // Create fresh entry
    this._genCounter++;
    const gen = this._genCounter;
    const wrapper = document.createElement('div');
    wrapper.className = 'convlog-wrapper';
    const historyDiv = document.createElement('div');
    historyDiv.id = 'output-history';
    const paneDiv = document.createElement('div');
    paneDiv.id = 'output-live';
    const lastInputDiv = document.createElement('div');
    lastInputDiv.className = 'last-input';
    lastInputDiv.style.display = 'none';
    wrapper.appendChild(historyDiv);
    wrapper.appendChild(paneDiv);
    wrapper.appendChild(lastInputDiv);

    entry = { wrapper, historyDiv, paneDiv, lastInputDiv, stableCount: 0, ws: null, gen };
    this._cache[title] = entry;

    this.container.innerHTML = '';
    this.container.className = '';
    this.container.appendChild(wrapper);

    // Fetch initial state from server
    let initialState = null;
    try {
      initialState = await api(`/${encodeURIComponent(title)}/history`);
      if (entry.gen !== gen) return; // stale
    } catch (e) {
      if (entry.gen !== gen) return;
      if (e.message && e.message.includes('Failed to fetch')) {
        this._showError('Cannot connect to Agent Manager server. Is it running?', title);
        return;
      }
    }

    if (entry.gen !== gen) return;

    // Render initial stable lines
    if (initialState && initialState.stable_lines && initialState.stable_lines.length > 0) {
      historyDiv.innerHTML = safeAnsiToHtml(initialState.stable_lines.join('\n'));
      entry.stableCount = initialState.stable_lines.length;
    }

    // Render initial pane
    if (initialState && initialState.pane && initialState.pane.length > 0) {
      paneDiv.innerHTML = safeAnsiToHtml(initialState.pane.join('\n'));
    }

    // Fire onContent for action button detection
    if (this.onContent) {
      const paneContent = initialState?.pane?.length > 0 ? initialState.pane.join('\n') : null;
      const tailContent = initialState?.stable_lines?.length > 0
        ? initialState.stable_lines.slice(-30).join('\n') : null;
      const content = paneContent || tailContent;
      if (content) this.onContent(content);
    }

    this._updateLastInput(entry, initialState ? initialState.last_input : '');
    this.container.scrollTop = this.container.scrollHeight;

    // Open WebSocket
    this._connectWs(title, entry);
  }

  _connectWs(title, entry) {
    if (entry.ws) { entry.ws.close(); entry.ws = null; }
    entry.gen = ++this._genCounter;
    const gen = entry.gen;

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    let ws;
    try {
      ws = new WebSocket(`${proto}//${location.host}/api/instances/${encodeURIComponent(title)}/ws`);
    } catch (e) {
      if (this.currentTitle === title) {
        this._showError('Cannot connect to Agent Manager server. Is it running?', title);
      }
      return;
    }
    entry.ws = ws;

    ws.onmessage = (e) => {
      if (entry.gen !== gen) { ws.close(); return; }
      const msg = JSON.parse(e.data);
      this._handleMessage(msg, title, entry);
    };

    ws.onerror = () => {
      if (entry.gen !== gen) return;
      if (this.currentTitle === title) {
        this._showError('Connection to server lost. The server may have stopped.', title);
      }
    };

    ws.onclose = () => {
      if (entry.gen === gen) entry.ws = null;
    };
  }

  // Disconnect from the currently displayed instance (hide it)
  disconnect() {
    this.currentTitle = null;
  }

  // Remove a title from the cache and close its WS
  evict(title) {
    const entry = this._cache[title];
    if (entry) {
      if (entry.ws) { entry.ws.close(); entry.ws = null; }
      entry.gen = -1; // invalidate any in-flight callbacks
      delete this._cache[title];
    }
  }

  _updateLastInput(entry, lastInput) {
    if (!entry.lastInputDiv) return;
    if (lastInput) {
      entry.lastInputDiv.style.display = '';
      entry.lastInputDiv.textContent = '\u276f ' + lastInput;
    } else {
      entry.lastInputDiv.style.display = 'none';
    }
  }

  _handleMessage(msg, title, entry) {
    const isVisible = (this.currentTitle === title);
    const wasAtBottom = isVisible &&
      (this.container.scrollHeight - this.container.scrollTop - this.container.clientHeight < 30);

    if (msg.type === 'history_append') {
      if (msg.lines && msg.lines.length > 0 && entry.historyDiv) {
        const newHtml = safeAnsiToHtml(msg.lines.join('\n'));
        const fragment = document.createElement('span');
        fragment.innerHTML = newHtml;

        if (entry.stableCount > 0) {
          entry.historyDiv.appendChild(document.createTextNode('\n'));
        }
        while (fragment.firstChild) {
          entry.historyDiv.appendChild(fragment.firstChild);
        }
        entry.stableCount += msg.lines.length;
      }
    } else if (msg.type === 'pane') {
      if (entry.paneDiv) {
        const hasContent = msg.content && msg.content.trim();
        entry.paneDiv.innerHTML = hasContent ? safeAnsiToHtml(msg.content) : '';
        entry.paneDiv.style.display = hasContent ? '' : 'none';
      }

      this._updateLastInput(entry, msg.last_input);

      // Notify about status changes (always, even for background instances)
      if (msg.status && this.onStatus) {
        this.onStatus(title, msg.status);
      }

      // Notify about content for mode detection (only for visible instance)
      if (isVisible && msg.content && this.onContent) {
        this.onContent(msg.content);
      }
    }

    if (isVisible && wasAtBottom) {
      this.container.scrollTop = this.container.scrollHeight;
    }
  }

  _showError(message, title) {
    this.container.innerHTML = `<div class="connection-error">
      <div class="error-icon">&#x26A0;</div>
      <div class="error-msg">${esc(message)}</div>
      <button class="retry-btn" onclick="window._convLogRetry && window._convLogRetry()">Retry</button>
    </div>`;
    window._convLogRetry = () => this.connect(title);
  }
}
