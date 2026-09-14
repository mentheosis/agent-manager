const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
function fixture(result){
 let Loader;
 const nodes={};
 const node=()=>({textContent:'',hidden:false,disabled:false,children:[],replaceChildren(){this.children=[];},append(n){this.children.push(n);}});
 vm.runInNewContext(fs.readFileSync('static/components/task_queues/am-task-queue-loader.js','utf8').replace(/^import.*$/m,''),{
  HTMLElement:class{},customElements:{define(_,cls){Loader=cls;}},queueRequest:async()=>result,
  document:{createElement:node,dispatchEvent(){}},CustomEvent:class{}
 });
 const loader=new Loader();loader.querySelector=k=>nodes[k]??=node();
 loader._instance={title:'queue'};loader._revision=1;
 loader.querySelector('textarea').value=JSON.stringify({tasks:[{key:'one'},{key:'two'}]});
 return {loader,nodes};
}
const task=key=>({key,task_type:'report',execution:{provider:'codex'},prompt:`Instructions for ${key}`,warnings:[]});
test('preview summarizes tasks and selects a single rendered assignment',async()=>{
 const {loader,nodes}=fixture({preview_hash:'hash',tasks:[task('one'),task('two')]});
 await loader.run('render');
 assert.match(nodes['.loader-message'].textContent,/2 tasks parsed · 2 rendered · 0 failed/);
 assert.equal(nodes['.preview-tasks'].children.length,2);
 assert.match(nodes['.rendered'].textContent,/Instructions for one/);
 loader.showTask(1);assert.match(nodes['.rendered'].textContent,/Instructions for two/);
 assert.equal(nodes['.enqueue'].disabled,false);
});
test('partial preview cannot be loaded',async()=>{
 const {loader,nodes}=fixture({tasks:[task('one')],error:'task two: invalid parameters'});
 await loader.run('render');assert.equal(nodes['.enqueue'].disabled,true);
 assert.match(nodes['.loader-message'].textContent,/1 tasks unresolved/);
});
test('successful load gives count and close action',async()=>{
 const {loader,nodes}=fixture({task_ids:[12,13]});loader._preview={preview_hash:'hash'};
 await loader.run('enqueue');
 assert.match(nodes['.loader-message'].textContent,/2 tasks loaded successfully/);
 assert.equal(nodes['.done'].hidden,false);
 assert.equal(nodes['.enqueue'].disabled,true);
});
