const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
let Panel;
vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../static/components/am-permissions-panel.js'), 'utf8').replace(/^import .*;$/gm, ''), {
    HTMLElement: class {},
    customElements: { define(name, cls) { Panel = cls; } },
    document: { createElement() { return {}; } },
});
function fixture() {
    const panel = new Panel();
    const nodes = {};
    panel.querySelector = selector => nodes[selector] ||= {
        children: [], replaceChildren() { this.children = []; },
        appendChild(child) { this.children.push(child); }, setAttribute() {},
        classList: { toggle() {} },
    };
    panel.provider = 'codex';
    panel.model = 'astra';
    panel.savedModel = 'astra';
    panel.reasoningOptions = { astra: { default: 'low', levels: [
        { effort: 'low', description: 'Fast' }, { effort: 'ultra', description: 'Automatic task delegation' },
    ] } };
    return { panel, nodes };
}
test('uses provider catalog, retains default, explains delegation', () => {
    const { panel, nodes } = fixture();
    panel.reasoningEffort = 'ultra';
    panel.populateReasoningOptions();
    assert.deepEqual(nodes['.perm-effort'].children.map(o => o.value), ['', 'low', 'ultra']);
    assert.equal(nodes['.perm-effort-hint'].textContent, 'Automatic task delegation');
    assert.equal(panel.invalidEffort, false);
});
test('incompatible model prevents save until a valid effort is selected', () => {
    const { panel, nodes } = fixture();
    panel.reasoningEffort = 'high';
    panel.populateReasoningOptions();
    panel.refreshDirty();
    assert.equal(nodes['.perm-apply-btn'].disabled, true);
    panel.reasoningEffort = '';
    panel.populateReasoningOptions();
    panel.refreshDirty();
    assert.equal(nodes['.perm-apply-btn'].disabled, false);
});
test('effort-only change applies next turn; other settings require restart', () => {
    const { panel, nodes } = fixture();
    panel.reasoningEffort = 'low';
    panel.populateReasoningOptions();
    panel.refreshDirty();
    assert.equal(nodes['.perm-apply-btn'].textContent, 'Apply to next turn');
    panel.memoryFile = '/new.md';
    panel.refreshDirty();
    assert.equal(nodes['.perm-apply-btn'].textContent, 'Restart and apply');
});
test('selector is hidden for Claude', () => {
    const { panel, nodes } = fixture();
    panel.provider = 'claude';
    panel.populateReasoningOptions();
    assert.equal(nodes['.perm-effort-section'].hidden, true);
});
test('active effort remains visible while a different effort awaits the next turn', () => {
    const { panel, nodes } = fixture();
    panel.reasoningEffort = 'ultra';
    panel.setActiveModel('astra', 'low');
    assert.equal(nodes['.model-current-value'].textContent, 'astra · low');
    assert.equal(nodes['.model-pending'].hidden, false);
    assert.equal(nodes['.model-pending'].textContent, 'Applies to next turn');
    panel.setActiveModel('astra', 'ultra');
    assert.equal(nodes['.model-pending'].hidden, true);
});
