import { safeAnsiToHtml, esc } from './ansi.js';
import { api } from './api.js';

// ConvLogView manages the terminal output display for a single instance.
// It owns the WS connection and renders stable history + volatile pane.
// DOM nodes are cached per instance so switching conversations is instant.
export class ConvLogView {
  constructor(container) {
    this.container = container;
    this.historyDiv = null;
    this.paneDiv = null;
    this.lastInputDiv = null;
    this.ws = null;
    this.generation = 0;
    this.stableCount = 0; // number of stable lines rendered
    this.onStatus = null; // callback(title, status) for sidebar updates
    this.onContent = null; // callback(content) for mode detection etc.
    this.currentTitle = null;
    this._cache = {}; // title -> { wrapper, historyDiv, paneDiv, lastInputDiv, stableCount }
  }

  // Connect to an instance: restore cached DOM or fetch fresh history + start WS
  async connect(title) {
    this._detachCurrent();
    if (this.ws) { this.ws.close(); this.ws = null; }
    this.generation++;
    const gen = this.generation;
    this.currentTitle = title;

    const cached = this._cache[title];
    if (cached) {
      // Restore cached DOM
      this.container.innerHTML = '';
      this.container.className = '';
      this.container.appendChild(cached.wrapper);
      this.historyDiv = cached.historyDiv;
      this.paneDiv = cached.paneDiv;
      this.lastInputDiv = cached.lastInputDiv;
      this.stableCount = cached.stableCount;

      // Fire onContent from current pane for action button detection
      if (this.onContent && this.paneDiv.textContent) {
        this.onContent(this.paneDiv.textContent);
      }

      this.container.scrollTop = this.container.scrollHeight;
    } else {
      // Set up fresh DOM structure
      this.container.innerHTML = '';
      this.container.className = '';
      const wrapper = document.createElement('div');
      wrapper.className = 'convlog-wrapper';
      this.historyDiv = document.createElement('div');
      this.historyDiv.id = 'output-history';
      this.paneDiv = document.createElement('div');
      this.paneDiv.id = 'output-live';
      this.lastInputDiv = document.createElement('div');
      this.lastInputDiv.className = 'last-input';
      this.lastInputDiv.style.display = 'none';
      wrapper.appendChild(this.historyDiv);
      wrapper.appendChild(this.paneDiv);
      wrapper.appendChild(this.lastInputDiv);
      this.container.appendChild(wrapper);
      this.stableCount = 0;

      // Fetch initial state from server
      let initialState = null;
      try {
        initialState = await api(`/${encodeURIComponent(title)}/history`);
        if (gen !== this.generation) return;
      } catch (e) {
        if (gen !== this.generation) return;
        if (e.message && e.message.includes('Failed to fetch')) {
          this._showError('Cannot connect to Agent Manager server. Is it running?', title);
          return;
        }
      }

      if (gen !== this.generation) return;

      // Render initial stable lines
      if (initialState && initialState.stable_lines && initialState.stable_lines.length > 0) {
        this.historyDiv.innerHTML = safeAnsiToHtml(initialState.stable_lines.join('\n'));
        this.stableCount = initialState.stable_lines.length;
      }

      // Render initial pane
      if (initialState && initialState.pane && initialState.pane.length > 0) {
        const paneContent = initialState.pane.join('\n');
        this.paneDiv.innerHTML = safeAnsiToHtml(paneContent);
      }

      // Fire onContent for action button detection
      if (this.onContent) {
        const paneContent = initialState?.pane?.length > 0 ? initialState.pane.join('\n') : null;
        const tailContent = initialState?.stable_lines?.length > 0
          ? initialState.stable_lines.slice(-30).join('\n') : null;
        const content = paneContent || tailContent;
        if (content) this.onContent(content);
      }

      this._updateLastInput(initialState ? initialState.last_input : '');
      this.container.scrollTop = this.container.scrollHeight;

      // Save to cache
      this._cache[title] = {
        wrapper, historyDiv: this.historyDiv, paneDiv: this.paneDiv,
        lastInputDiv: this.lastInputDiv, stableCount: this.stableCount,
      };
    }

    // Open WebSocket — server seeds from current raw stable count automatically
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    let newWs;
    try {
      newWs = new WebSocket(`${proto}//${location.host}/api/instances/${encodeURIComponent(title)}/ws`);
    } catch (e) {
      if (gen !== this.generation) return;
      this._showError('Cannot connect to Agent Manager server. Is it running?', title);
      return;
    }

    if (gen !== this.generation) {
      newWs.close();
      return;
    }
    this.ws = newWs;

    newWs.onmessage = (e) => {
      if (gen !== this.generation) { newWs.close(); return; }
      const msg = JSON.parse(e.data);
      this._handleMessage(msg, gen, title);
    };

    newWs.onerror = () => {
      if (gen !== this.generation) return;
      this._showError('Connection to server lost. The server may have stopped.', title);
    };

    newWs.onclose = () => {
      if (gen === this.generation) this.ws = null;
    };
  }

  // Detach current DOM into cache (preserving rendered content)
  _detachCurrent() {
    if (this.currentTitle && this._cache[this.currentTitle]) {
      this._cache[this.currentTitle].stableCount = this.stableCount;
    }
    this.historyDiv = null;
    this.paneDiv = null;
    this.lastInputDiv = null;
    this.currentTitle = null;
  }

  // Remove a title from the cache (e.g. when instance is killed)
  evict(title) {
    delete this._cache[title];
  }

  disconnect() {
    if (this.ws) { this.ws.close(); this.ws = null; }
    this._detachCurrent();
    this.stableCount = 0;
  }

  _updateLastInput(lastInput) {
    if (!this.lastInputDiv) return;
    if (lastInput) {
      this.lastInputDiv.style.display = '';
      this.lastInputDiv.textContent = '\u276f ' + lastInput;
    } else {
      this.lastInputDiv.style.display = 'none';
    }
  }

  _handleMessage(msg, gen, title) {
    if (gen !== this.generation) return;

    // console.log('msg', msg.type, { msg, gen, title });

    const wasAtBottom = this.container.scrollHeight - this.container.scrollTop - this.container.clientHeight < 30;

    if (msg.type === 'history_append') {
      // Append new stable lines to historyDiv using DOM insertion (not innerHTML +=)
      if (msg.lines && msg.lines.length > 0 && this.historyDiv) {
        const newHtml = safeAnsiToHtml(msg.lines.join('\n'));
        const fragment = document.createElement('span');
        fragment.innerHTML = newHtml;

        if (this.stableCount > 0) {
          // Add a newline text node before appending new content
          this.historyDiv.appendChild(document.createTextNode('\n'));
        }
        // Move all child nodes from fragment into historyDiv
        while (fragment.firstChild) {
          this.historyDiv.appendChild(fragment.firstChild);
        }
        this.stableCount += msg.lines.length;
      }
    } else if (msg.type === 'pane') {
      // Replace volatile pane content
      if (this.paneDiv) {
        const hasContent = msg.content && msg.content.trim();
        this.paneDiv.innerHTML = hasContent ? safeAnsiToHtml(msg.content) : '';
        this.paneDiv.style.display = hasContent ? '' : 'none';
      }

      this._updateLastInput(msg.last_input);

      // Notify about status changes
      if (msg.status && this.onStatus) {
        this.onStatus(title, msg.status);
      }

      // Notify about content for mode detection
      if (msg.content && this.onContent) {
        this.onContent(msg.content);
      }
    }

    if (wasAtBottom) this.container.scrollTop = this.container.scrollHeight;
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
