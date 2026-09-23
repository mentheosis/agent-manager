const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
function load(file, globals={}) {
  let cls;
  const context={HTMLElement:class {},customElements:{define(name,value){cls=value;}},...globals};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname,'../static/components',file),'utf8').replace(/^import[\s\S]*?;$/gm,''),context);
  return cls;
}
function panelFixture(request) {
  const Panel=load('task_queues/am-task-queue-panel.js',{queueRequest:request,document:{dispatchEvent(){}},CustomEvent:class{}});
  const panel=new Panel();
  const nodes={};
  panel.querySelector=key=>nodes[key]??=( {textContent:'',value:'2',setAttribute(key,value){this[key]=value;}} );
  panel.querySelectorAll=()=>[];
  panel._instance={title:'queue'};
  return {panel,nodes};
}
test('queue panel preserves unsaved edits while showing persisted capacity',async()=>{
 let limit=1;
 const {panel,nodes}=panelFixture(async()=>({state:'running',max_workers_ceiling:8,queue:{max_workers:limit,available_tasks:12,active_tasks:1,active_workers:1}}));
 await panel.load();
 assert.equal(nodes.input.value,1);
 nodes.input.value='2';
 await panel.load();
 assert.equal(nodes.input.value,'2');
 assert.equal(nodes['.queue-effective'].textContent,'Effective limit: 1');
 assert.equal(nodes['[data-count=workers]'].textContent,'1 / 1');
 limit=2;await panel.load();
 assert.equal(nodes['.queue-effective'].textContent,'Effective limit: 2');
});
test('failed capacity save leaves effective limit unchanged and exposes failure',async()=>{
 const {panel,nodes}=panelFixture(async(title,path)=>{
  if(path==='control') throw new Error('Database unavailable');
  return {state:'paused',queue:{max_workers:1,available_tasks:4,active_tasks:0,active_workers:0}};
 });
 await panel.load();nodes.input.value='2';
 await panel.control('max_workers');
 assert.equal(nodes['.queue-effective'].textContent,'Effective limit: 1');
 assert.equal(nodes['.queue-error'].textContent,'Database unavailable');
});
test('late status response cannot overwrite a different selected queue',async()=>{
 let finish;
 const {panel,nodes}=panelFixture(()=>new Promise(resolve=>finish=resolve));
 const pending=panel.load();panel._instance={title:'different'};
 finish({state:'running',queue:{max_workers:8}});await pending;
 assert.equal(Object.keys(nodes).length,0);
});

test('stopped queue displays profile limits and queue identifier without overwriting edits', async()=>{
 const {panel,nodes}=panelFixture(async()=>({state:'stopped',queue:null,settings:{queue_id:'audit',max_workers:2,limits:{lease_secs:90,task_limit_secs:7200,task_limit_tokens:300000}}}));
 await panel.load();
 assert.equal(nodes['[name=lease]'].value,90);
 assert.equal(nodes['[name=seconds]'].value,7200);
 assert.equal(nodes['[name=tokens]'].value,300000);
 assert.equal(nodes['.queue-state'].textContent,'Controller stopped');
 assert.equal(nodes['.queue-state']['data-state'],'stopped');
 nodes['[name=lease]'].value='120';
 await panel.load();
 assert.equal(nodes['[name=lease]'].value,'120');
});
test('task activity counts the table snapshot while controller is stopped',async()=>{
 const {panel,nodes}=panelFixture(async()=>({state:'stopped',settings:{max_workers:2},queue:null}));
 panel._onTasks({detail:{title:'queue',tasks:[{status:'queued'},{status:'running'},{status:'claimed'},{status:'completed'},{status:'awaiting_review'}]}});
 await panel.load();
 assert.equal(nodes['[data-count=queued]'].textContent,1);
 assert.equal(nodes['[data-count=active]'].textContent,2);
 assert.equal(nodes['[data-count=completed]'].textContent,1);
 assert.equal(nodes['[data-count=other]'].textContent,1);
 panel._onTasks({detail:{title:'different',tasks:[]}});
 assert.equal(nodes['[data-count=queued]'].textContent,1);
 panel._onTasks({detail:{title:'queue',tasks:[]}});
 assert.equal(nodes['[data-count=queued]'].textContent,0);
});
test('workflow badges distinguish completion, attention, activity and idle',()=>{
 const Tasks=load('task_queues/am-task-queue-tasks.js');
 const view=new Tasks();
 const status=(...states)=>view.workflowStatus(states.map(status=>({status}))).state;
 assert.equal(status('queued'),'idle');
 assert.equal(status('completed','queued'),'idle');
 assert.equal(status('running','queued'),'running');
 assert.equal(status('submitted'),'running');
 assert.equal(status('completed','completed'),'completed');
 assert.equal(status('awaiting_review','running'),'running');
 assert.equal(status('failed','running'),'mixed');
 assert.equal(status('blocked'),'blocked');
 assert.equal(status('blocked','reviewing'),'mixed');
 assert.equal(status('reviewing','queued'),'running');
 assert.equal(status('blocked','queued'),'blocked');
 assert.equal(status('awaiting_review'),'awaiting-human');
 assert.equal(status('cancelled'),'blocked');
 assert.equal(status(),'idle');
});

test('expanded rows survive conversation changes and remain isolated per controller',()=>{
 const Tasks=load('task_queues/am-task-queue-tasks.js');
 const view=new Tasks();
 view._instance={instance_id:'one',title:'first'};
 view.expandedRows().add('workflow:pilot');view.expandedRows().add('task:1');
 view._instance=null;
 view._instance={instance_id:'two',title:'second'};
 assert.equal(view.expandedRows().size,0);
 view.expandedRows().add('task:2');
 view._instance={instance_id:'one',title:'renamed'};
 assert.deepEqual([...view.expandedRows()],['workflow:pilot','task:1']);
 view.expandedRows().delete('workflow:pilot');
 view._instance={instance_id:'two',title:'second'};
 assert.deepEqual([...view.expandedRows()],['task:2']);
 view._instance={instance_id:'one',title:'renamed'};
 assert.equal(view.expandedRows().has('workflow:pilot'),false);
});

test('blocked reasons distinguish controller budgets from task blockers and attempt caps',()=>{
 const Tasks=load('task_queues/am-task-queue-tasks.js');
 const tasks=new Tasks();
 assert.equal(tasks.statusReason({status:'blocked',latest_error:'Token budget exhausted; resume from saved evidence'}).label,'Token limit reached');
 assert.equal(tasks.statusReason({status:'blocked',latest_error:'Time budget exhausted: controller limit 120 seconds'}).label,'Time limit reached');
 const blocker=tasks.statusReason({status:'blocked',latest_error:'Submitted outcome recorded',latest_result:{blockers:['Need approved archive credentials'],summary:'Blocked'}});
 assert.equal(blocker.detail,'Need approved archive credentials');
 assert.equal(tasks.statusReason({status:'running',latest_error:'old error'}),null);
 assert.match(tasks.retryUnavailable({attempt_count:3,max_attempts:3}),/3\/3 attempts used/);
 assert.equal(tasks.retryUnavailable({attempt_count:1,max_attempts:3}),'');
});

test('Resume is available for budget-blocked reviewed attempts even at the retry cap',()=>{
 const Tasks=load('task_queues/am-task-queue-tasks.js'); const tasks=new Tasks();
 const task={status:'blocked',rounds:[{}],latest_error:'Time budget exhausted: controller limit 3600 seconds',attempt_count:3,max_attempts:3};
 assert.equal(tasks.canResume(task),true);
 assert.match(tasks.retryUnavailable(task),/3\/3/);
 assert.equal(tasks.canResume({...task,status:'running'}),false);
 assert.equal(tasks.canResume({...task,latest_error:'Submitted outcome recorded'}),false);
 assert.equal(tasks.canResume({...task,rounds:[]}),false);
 assert.equal(tasks.canResume({...task,latest_error:'Controller token budget reserved for review: 900 used / 1000 allowed'}),true);
});

test('review round limit appears in live controls and enables Resume after limit block',async()=>{
 let saved;
 const {panel,nodes}=panelFixture(async(title,route,body)=>{
  if(body) saved=body;
  return {state:'running',queue:{max_workers:1,limits:{lease_secs:60,task_limit_secs:3600,task_limit_tokens:2000000,review_round_limit:12}}};
 });
 await panel.load();
 assert.equal(nodes['[name=rounds]'].value,12);
 nodes['[name=rounds]'].value='24';
 await panel.control('limits');
 assert.equal(saved.limits.review_round_limit,24);
 const Tasks=load('task_queues/am-task-queue-tasks.js');
 assert.equal(new Tasks().canResume({status:'blocked',rounds:[{}],latest_error:'Review round limit reached; resume with saved findings'}),true);
});

test('conversation prompt errors preserve drafts and concurrent submissions are ignored', async()=>{
 let reject;
 const Pane=load('am-terminal-pane.js',{api:{sendPrompt:()=>new Promise((_,r)=>reject=r)}});
 const pane=new Pane();
 const input={value:'Continue researching'};
 pane._instance={title:'worker'};
 pane.querySelector=()=>input;
 const notes=[];
 pane.appendNote=text=>notes.push(text);
 pane.clearPendingImages=()=>{throw new Error('Failed send must retain attachments');};
 const pending=pane.submitPrompt();
 await pane.submitPrompt();
 reject(new Error('Increase the token limit'));
 await pending;
 assert.equal(input.value,'Continue researching');
 assert.match(notes[0],/Increase the token limit/);
 assert.equal(pane._sendingPrompt,false);
});

test('independent queue conversation is labelled after successful send', async()=>{
 const Pane=load('am-terminal-pane.js',{api:{sendPrompt:async()=>({ok:true,queue_managed:false})}});
 const pane=new Pane();
 const input={value:'Explain the old finding'};
 pane._instance={title:'old-reviewer'};
 pane.querySelector=()=>input;
 pane.clearPendingImages=()=>{};
 const notes=[];pane.appendNote=text=>notes.push(text);
 await pane.submitPrompt();
 assert.equal(input.value,'');
 assert.match(notes[0],/outside the task queue/);
});

test('stopped conversation shows a clear resumable pause reason',()=>{
 const Tasks=load('task_queues/am-task-queue-tasks.js');
 const view=new Tasks();
 const reason=view.statusReason({status:'blocked', latest_error:'Paused by operator: conversation stopped. Send a message to resume.'});
 assert.equal(reason.label,'Paused by you');
 assert.match(reason.detail,/Send a message/);
});

test('sidebar queue rollup prioritizes human gates over running workflows',()=>{
 const Sidebar=load('am-sidebar.js');
 const view=new Sidebar();
 const status=(...states)=>view.queueRollup(states.map(status=>({status}))).state;
 assert.equal(status('awaiting_review','running'),'waiting-human');
 assert.equal(status('blocked','running'),'running');
 assert.equal(status('blocked','queued'),'blocked');
 assert.equal(status('completed','completed'),'completed');
 assert.equal(status('completed','queued'),'idle');
 assert.equal(status(),'idle');
 assert.equal(view.queueRollup(null).state,'unknown');
 assert.equal(view.queueRollup([{status:'running'},{status:'blocked',latest_error:'Paused by operator: stopped'}]).state,'waiting-human');
});

test('sidebar polls every queue and all task pages without selecting a controller',async()=>{
 const requests=[];
 const Sidebar=load('am-sidebar.js',{queueRequest:async(title,path)=>{
  requests.push([title,path]);
  if(title==='unavailable') throw new Error('offline');
  if(path==='tasks?offset=0') return Array.from({length:100},()=>({status:'completed'}));
  return [{status:'awaiting_review'}];
 }});
 const view=new Sidebar();
 view.isConnected=true;
 view._instances=[{title:'background',instance_id:'one',controller_mode:'task_queue'},
  {title:'unavailable',instance_id:'two',controller_mode:'task_queue'},{title:'normal'}];
 view.querySelectorAll=()=>[];
 await view.refreshQueueStatuses();
 assert.equal(view._queueStatuses.get('background').state,'waiting-human');
 assert.equal(view._queueStatuses.get('unavailable').state,'unknown');
 assert.equal(requests.length,3);
 assert.ok(requests.some(([title,path])=>title==='background'&&path==='tasks?offset=100'));
});

test('sidebar ignores status responses for removed or replaced controllers',async()=>{
 let finish;
 const Sidebar=load('am-sidebar.js',{queueRequest:()=>new Promise(resolve=>finish=resolve)});
 const view=new Sidebar();
 view.isConnected=true;
 view._instances=[{title:'queue',instance_id:'old',controller_mode:'task_queue'}];
 view.querySelectorAll=()=>[];
 const pending=view.refreshQueueStatuses();
 view._instances=[{title:'queue',instance_id:'new',controller_mode:'task_queue'}];
 finish([{status:'running'}]);
 await pending;
 assert.equal(view._queueStatuses.size,0);
});

test('reviewers group by controller and attempt without changing ownership',()=>{
 const Sidebar=load('am-sidebar.js'), view=new Sidebar();
 const queue={title:'queue',controller_mode:'task_queue'};
 const worker={title:'worker',parent:'queue',queue_attempt:{root_attempt:'a',role:'worker'}};
 const reviewer={title:'reviewer',parent:'queue',queue_attempt:{root_attempt:'a',role:'reviewer',round:2}};
 const early={title:'early',parent:'queue',queue_attempt:{root_attempt:'a',role:'reviewer',round:1}};
 const retry={title:'retry',parent:'queue',queue_attempt:{root_attempt:'b',role:'worker'}};
 const orphan={title:'orphan',parent:'queue',queue_attempt:{root_attempt:'missing',role:'reviewer'}};
 view._instances=[queue,worker,reviewer,retry,early,orphan];
 const tree=view.sidebarChildren();
 assert.equal(tree.get('worker').map(i=>i.title).join(','),'early,reviewer');
 assert.equal(tree.get('queue').map(i=>i.title).join(','),'worker,retry,orphan');
 assert.equal(reviewer.parent,'queue');
 view._instances=view._instances.filter(i=>i!==worker);
 assert.ok(view.sidebarChildren().get('queue').includes(reviewer));
});

test('selecting a reviewer reveals ancestors and preserves other worker expansions',()=>{
 const Sidebar=load('am-sidebar.js'),view=new Sidebar();
 view._instances=[{title:'queue',controller_mode:'task_queue',folder:'audits'},
 {title:'worker',parent:'queue',queue_attempt:{root_attempt:'a',role:'worker'}},
 {title:'review',parent:'queue',queue_attempt:{root_attempt:'a',role:'reviewer'}}];
 view.render=()=>{};view.updateSelection=()=>{};
 view._expandedReviews.add('other');
 view.selectedTitle='review';
 assert.ok(view._expandedTeams.has('queue'));
 assert.ok(view._expandedReviews.has('worker'));
 assert.ok(view._expandedReviews.has('other'));
 assert.ok(view._expandedFolders.has('audits'));
 view._expandedReviews.delete('worker');
 view.selectedTitle='review';
 assert.equal(view._expandedReviews.has('worker'),false);
});

test('sidebar ready conversations age to idle without mutating stream state',()=>{
 const Sidebar=load('am-sidebar.js'),view=new Sidebar();
 const now=Date.parse('2026-09-23T12:00:00Z');
 const stream={status:'ready',eventHistory:[{type:'result',ts:'2026-09-23T11:29:59Z'}]};
 assert.equal(view.conversationDisplayStatus({},stream,now),'idle');
 assert.equal(stream.status,'ready');
 stream.eventHistory.push({type:'connection',ts:'2026-09-23T12:00:00Z'});
 assert.equal(view.conversationDisplayStatus({},stream,now),'idle');
 stream.eventHistory.push({type:'assistant_text',ts:'2026-09-23T11:59:00Z'});
 assert.equal(view.conversationDisplayStatus({},stream,now),'ready');
 stream.status='running';
 assert.equal(view.conversationDisplayStatus({},stream,now+3600000),'running');
 assert.equal(view.conversationDisplayStatus({status:'ready'},null,now),'ready');
 assert.equal(view.conversationDisplayStatus({}, {status:'ready',eventHistory:[{ts:'2026-09-23T11:30:00Z'}]},now),'ready');
});

test('sidebar uses persisted activity age immediately despite fresh restart events',()=>{
 const Sidebar=load('am-sidebar.js'),view=new Sidebar();
 const now=Date.parse('2026-09-23T12:00:00Z');
 const stream={status:'ready',eventHistory:[
  {type:'assistant_text',ts:'2026-09-22T12:00:00Z'},
  {type:'result',ts:'2026-09-22T12:01:00Z'},
  {type:'system_init',ts:'2026-09-23T11:59:00Z'},
  {type:'status',status:'ready',ts:'2026-09-23T11:59:01Z'}
 ]};
 assert.equal(view.conversationDisplayStatus({},stream,now),'idle');
 stream.eventHistory.push({type:'tool_result',ts:'2026-09-23T11:59:59Z'});
 assert.equal(view.conversationDisplayStatus({},stream,now),'ready');
 assert.equal(view.conversationDisplayStatus({status:'ready',created_at:'2026-09-22T12:00:00Z'},{eventHistory:[]},now),'idle');
});
