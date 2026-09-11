const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
function load(file, globals = {}) {
    let component;
    const context = {document: {createElement: element}, ...globals};
    for (const dependency of ['am-controller-activity.js','teams/view-config.js','task_queues/view-config.js']) {
        vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../static/components', dependency), 'utf8').replace(/export /g, '') + (dependency.includes('view-config') ? `; globalThis.${dependency.startsWith('teams') ? 'teamFilters' : 'queueFilters'} = ${dependency.startsWith('teams') ? 'teamFilters' : 'queueFilters'};` : ''), context);
    }
    vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../static/components', file), 'utf8').replace(/^import[\s\S]*?;$/gm, ''), {
        HTMLElement: class {}, customElements: {define(name, cls) {component = cls;}}, ...context,
    });
    return component;
}
function element() {
    return {children: [], dataset: {}, scrollHeight: 0, scrollTop: 0, clientHeight: 0,
        append(...items) {this.children.push(...items);}, appendChild(item) {this.children.push(item);},
        querySelector() {return null;}};
}

test('loop pane ignores old Claude events and replays reconnect delta once', () => {
    const history = [];
    const Component = load('teams/am-team-activity.js', {document: {createElement: element}, streamManager: {get: () => ({eventHistory: history})}});
    const pane = new Component();
    const output = element();
    pane._instance = {title: 'team'};
    pane.querySelector = () => output;
    pane.renderEvent({type: 'system_init', data: {model: 'claude'}});
    pane.renderEvent({type: 'status', status: 'ready'});
    assert.equal(output.children.length, 0);
    const event = {type: 'team_event', seq: 1, actor: 'leader', target: 'worker', event_type: 'tool_use', text: '<script>not HTML</script>'};
    pane.renderEvent(event);
    history.push(event, {...event, seq: 2, text: 'Worker completed'});
    pane.renderEvent({type: 'connection', status: 'reconnected'});
    assert.equal(output.children.length, 2);
    assert.match(output.children[0].children[0].children[0].textContent, /leader → worker/);
    assert.equal(output.children[0].children[1].textContent, '<script>not HTML</script>');
});

test('conversation routes loop parents to activity view and agents to terminal', () => {
    const App = load('am-app.js');
    const nodes = Object.fromEntries(['am-loop-pane', 'am-terminal-pane', 'am-team-panel'].map(name => [name, {classList: {toggle(key, value) {this[key] = value;}}}]));
    const app = Object.create(App.prototype);
    app.querySelector = name => nodes[name];
    app.activeTab = 'terminal';
    app.currentInst = {title: 'team', kind: 'loop', provider: 'claude'};
    app.updateTeamPanel();
    assert.equal(nodes['am-loop-pane'].classList.active, true);
    assert.equal(nodes['am-terminal-pane'].classList.active, false);
    assert.equal(nodes['am-loop-pane'].instance.title, 'team');
    app.currentInst = {title: 'worker', kind: 'agent', provider: 'claude'};
    app.updateTeamPanel();
    assert.equal(nodes['am-loop-pane'].classList.active, false);
    assert.equal(nodes['am-terminal-pane'].classList.active, true);
    assert.equal(nodes['am-loop-pane'].instance, null);
    app.currentInst = {title: 'team', instance_type: 'loop'};
    app.activeTab = 'settings';
    app.updateTeamPanel();
    assert.equal(nodes['am-loop-pane'].classList.active, false);
    assert.equal(nodes['am-team-panel'].instance, null);
});


test('team filters affect existing and arriving events without changing agent filters', () => {
    const Pane = load('teams/am-team-activity.js', {document: {createElement: element}});
    const pane = new Pane();
    const output = element();
    pane.querySelector = () => output;
    pane.querySelectorAll = () => output.children;
    pane.renderEvent({type: 'team_event', seq: 1, event_type: 'status', text: 'ready'});
    pane._onFilterChanged({detail: {scope: 'team', filters: {status: false}}});
    assert.equal(output.children[0].hidden, true);
    pane.renderEvent({type: 'team_event', seq: 2, event_type: 'status', text: 'running'});
    assert.equal(output.children[1].hidden, true);
    pane._onFilterChanged({detail: {scope: 'agent', filters: {status: true}}});
    assert.equal(output.children[0].hidden, true);
    pane._onFilterChanged({detail: {scope: 'team', filters: {status: true}}});
    assert.equal(output.children[0].hidden, false);
    assert.equal(output.children[1].hidden, false);
    const Toolbar = load('am-toolbar.js');
    const toolbar = new Toolbar();
    const menu = {};
    toolbar.querySelector = () => menu;
    toolbar._instance = {kind: 'loop'};
    toolbar._teamFilters.status = false;
    toolbar.renderFilters();
    assert.match(menu.innerHTML, /Controller events/);
    assert.doesNotMatch(menu.innerHTML, /Thinking/);
    assert.equal(toolbar.filters.status, false);
    toolbar._instance = {kind: 'agent'};
    toolbar.renderFilters();
    assert.match(menu.innerHTML, /Thinking/);
    assert.equal(toolbar.filters.assistant_text, true);
});

test('collapsed teams stay hidden while actual orphans remain visible', () => {
    const Sidebar = load('am-sidebar.js');
    const sidebar = Object.create(Sidebar.prototype);
    sidebar._unsubscribers = [];
    sidebar._expandedTeams = new Set();
    sidebar._expandedFolders = new Set();
    sidebar._instances = [{title: 'team', kind: 'loop'}, {title: 'child', parent: 'team'}, {title: 'orphan', parent: 'missing'}];
    const list = element(), mini = element();
    sidebar.querySelector = key => key === '#instance-list' ? list : mini;
    sidebar.createInstanceItem = inst => inst.title;
    sidebar.createMiniItem = inst => inst.title;
    sidebar.updateSelection = () => {};
    sidebar.render();
    assert.deepEqual(list.children, ['team', 'orphan']);
    sidebar._expandedTeams.add('team');
    list.children = [];
    sidebar.render();
    assert.deepEqual(list.children, ['team', 'child', 'orphan']);
    sidebar._expandedTeams.clear();
    list.children = [];
    sidebar.render();
    assert.deepEqual(list.children, ['team', 'orphan']);
});
