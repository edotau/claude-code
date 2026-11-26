const fs = require('fs'), vm = require('vm'), assert = require('assert'), crypto = require('crypto');
const {sources,manifest,expected} = JSON.parse(fs.readFileSync(0,'utf8'));
for (const [file, source] of Object.entries(sources)) {
 const declared = [...source.matchAll(/^function ([\w$]+)\(/gm)].map(m=>m[1]).sort();
 const listed = manifest.filter(m=>m.file===file).map(m=>m.name).sort();
 assert.deepStrictEqual(listed,declared,file+' fixture manifest must cover every accessor');
}
function context(i) {
 const f={file:'internal/example/a.go',line:7,symbol:'Calculate',title:'wrong total',symptom:'wrong total',scenario:'zero input',evidence:'observed',kind:['bug','perf','duplicate','dead'][i%4]};
 return {CWD:i%2?'/work/team\'s repo':'',CD:i%2?'cd /work/repo && ':'',a:{focus:i%3?'tests':'',cwd:i%2?'/work/team\'s repo':'',rules:i%3?'owner override':''},EXT:i%2?[{agent:'pi',profile:'dev',provider:'databricks'},{agent:'copilot'}]:[],
 PATHS:i%2?['internal/example','cmd/example']:[],PKGS:['internal/example'],FILES_MODE:!!(i%2),RANGE:['','HEAD~1..HEAD','start...finish','start..'][i%4],SPLIT:!!(i%3),g:['internal/example'],
 dryRun:!!(i%2),json:'\nJSON:\n{"report":"fixture"}',label:'fixture',FOCUS:' focus',IN:' in fixture',SHARED_TREE:'shared rules',TARGET:'fixture target',REFUTE_RULES:'refute rules',RULES:'fixture rules',
 HOT:['internal/agent','internal/settings'],fs:[f],pkg:'internal/example',relPath:p=>p.replace('/work/repo/',''),TASK:'fix the fixture',KIND:['feature','fix','refactor','consolidate'][i%4],focus:i%2?' focused':'',implTargets:['internal/example/a.go'],
 argv:['claude-code','plan','--name','a b'],note:'fixture note',need:[{file:'internal/example/a.go',need:'wire caller'}],w:2,LINE_BUDGET:'neutral',ST_BUDGET:{neutral:20},implOver:()=>[['internal/example',23]],IMPL_FILES:['internal/example/a.go'],ST_PROPOSE:' propose tests',dirs:['internal/example'],stDiff:ps=>'diff '+ps.join(' '),res:{report:'fixture report',decisions:['keep behavior']},
 JOB:i%2?'consolidate':'fix',FILES:i%3?{'internal/example':['internal/example/a.go']}: {},l:{ask:i%2?'test-quality':'correctness'},p:'internal/example',f,x:{f},stCost:f=>!!f.kind&&f.kind!=='bug',MERGE_KINDS:['duplicate','dead','legacy','stale-test'],
 BASE:'/fixtures/base',SKILL:'/fixtures/skill.md',OFFLINE:'offline fixture',TASKS:{},GROUND_TRUTH:{},setupLines:['mkdir -p fixture','echo fixture']};
}
function args(m,c,i) {
 if(m.original==='scopeCmd')return[c.g];
 if(['findFiles','findPrompt'].includes(m.original))return[{group:i%3?c.g:[],dim:{prompt:'correctness'},chunk:{files:i%3?[{path:'internal/example/a.go'}]:[]}}];
 if(m.original==='refuteBrief')return[c.pkg,i%3?c.fs:[]];
 if(m.original==='ro')return[i%2?'tests':'logic'];
 if(m.original==='judge')return[c.f];
 if(m.original==='findAsk')return[c.l];
 if(['executorPrompt','judgePrompt'].includes(m.original))return[{run:'s1-method1',scenario:['s1','s2','s9','s10','s11','s13','s14'][Math.floor(i/2)%7],condition:i%2?'method':'control'},i%3?'fixture report':null];
 return [];
}
function normalized(v) {if(v&&typeof v==='object')return Object.fromEntries(Object.entries(v).map(([k,x])=>[k,typeof x==='function'?x('/fixtures/team\'s run'):x]));return v}
const results=[];
for(const file of [...new Set(manifest.map(m=>m.file))])for(let i=0;i<28;i++) {
 const c=context(i), ms=manifest.filter(m=>m.file===file);
 Object.assign(c,vm.runInNewContext(sources[file]+'\n; ({'+ms.map(m=>m.name).join(',')+'})',{}));
 for(const [label,name] of Object.entries({TASKS:'evaluatorTasks',GROUND_TRUTH:'evaluatorGroundTruth',OFFLINE:'evaluatorOffline',REFUTE_RULES:'analyzerRefuteRules',SHARED_TREE:'analyzerSharedTree',RULES:file+'Rules',FOCUS:file+'Focus'})) {
  const m=ms.find(m=>m.name===name);if(m)c[label]=c[m.name](...(m.deps.length?[Object.fromEntries(m.deps.map(n=>[n,c[n]]))]:[]));
 }
 for(const m of ms){const value=c[m.name](...(m.deps.length?[Object.fromEntries(m.deps.map(n=>[n,c[n]]))]:[]),...args(m,c,i));
  results.push({file,name:m.name,case:i,sha256:crypto.createHash('sha256').update(JSON.stringify(normalized(value))).digest('hex')});}

}
assert.deepStrictEqual(results,expected);
console.log(results.length+' prompt outputs match pre-migration SHA-256 fixtures');
