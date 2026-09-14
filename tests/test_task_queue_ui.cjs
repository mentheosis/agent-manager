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
 assert.equal(status('running','queued'),'active');
 assert.equal(status('submitted'),'active');
 assert.equal(status('completed','completed'),'completed');
 assert.equal(status('awaiting_review','running'),'awaiting-human');
 assert.equal(status('failed','running'),'blocked');
 assert.equal(status('blocked'),'blocked');
 assert.equal(status('cancelled'),'blocked');
 assert.equal(status(),'idle');
});
