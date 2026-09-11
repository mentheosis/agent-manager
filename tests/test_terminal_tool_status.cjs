// Run with: node --test tests/test_terminal_tool_status.cjs
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Load the component with browser globals stubbed; exercise its event handler.
let Component;
vm.runInNewContext(
    fs.readFileSync(path.join(__dirname, '../static/components/am-terminal-pane.js'), 'utf8')
        .replace(/^import .*;$/gm, ''),
    { HTMLElement: class {}, customElements: { define(name, cls) { Component = cls; } }, CSS: { escape: s => s } },
);

function fixture() {
    const classes = new Set(['pending']);
    const status = {
        title: 'Pending',
        classList: {
            remove(...names) { names.forEach(name => classes.delete(name)); },
            add(name) { classes.add(name); },
        },
    };
    const tool = { querySelector: selector => selector === '.tool-status' ? status : null, appendChild() {} };
    const pane = Object.create(Component.prototype);
    Object.assign(pane, {
        checkScrollPosition() {}, autoScroll() {},
        querySelectorAll(selector) {
            return selector === '.tool-status.pending' ? (classes.has('pending') ? [status] : []) : [tool];
        },
        getOrCreateCurrentTurnBody: () => ({ appendChild() {} }),
        isDuplicateAssistantText: () => false, isDuplicateResult: () => false,
        createEventElement: () => ({}), createToolResultElement: () => ({}),
    });
    return { pane, classes, status };
}

test('terminal completion marks missing results unavailable; late results resolve them', () => {
    for (const isError of [false, true]) {
        const { pane, classes, status } = fixture();
        pane.appendEventToCurrentTurn({ type: 'result', terminal: true });
        assert.deepEqual([...classes], ['unavailable']);
        assert.match(status.title, /result unavailable/);
        pane.appendEventToCurrentTurn({ type: 'tool_result', tool_id: 'call-1', is_error: isError });
        assert.deepEqual([...classes], [isError ? 'error' : 'success']);
    }
});

test('nonterminal completion leaves asynchronous tools pending', () => {
    const { pane, classes } = fixture();
    pane.appendEventToCurrentTurn({ type: 'result' });
    assert.deepEqual([...classes], ['pending']);
});

test('matching output resolves a pending tool successfully', () => {
    const { pane, classes } = fixture();
    pane.appendEventToCurrentTurn({ type: 'tool_result', tool_id: 'call-1', is_error: false });
    assert.deepEqual([...classes], ['success']);
});
