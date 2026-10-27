'use strict';
const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const http = require('node:http');
const zlib = require('node:zlib');
const crypto = require('node:crypto');
const {spawn} = require('node:child_process');
const {binary, environment} = require('./isolated-env.cjs');
const {prepareCopy, sha256} = require('../lib/native-patch.cjs');
let prepared, originalHash;
const reports = [];
const sandboxRoot = path.join(__dirname,'.sandbox','integration');
const prompt = 'WORKBUDDY_GZIP_SYNTHETIC_ONLY\nUnicode: 中文🙂; JSON: {"role":"tool","value":"`a\\nb`"}\n' + 'transport-only fixture '.repeat(3000);

function events(res, {text='ok',tool=null}={}) {
  res.setHeader('content-type','text/event-stream');
  const message={id:'msg_synthetic',type:'message',role:'assistant',model:'claude-sonnet-4-6',content:[],stop_reason:null,stop_sequence:null,usage:{input_tokens:20,output_tokens:1}};
  emit(res,'message_start',{type:'message_start',message});
  if(tool){
    emit(res,'content_block_start',{type:'content_block_start',index:0,content_block:{type:'tool_use',id:'toolu_synthetic',name:'Read',input:{}}});
    emit(res,'content_block_delta',{type:'content_block_delta',index:0,delta:{type:'input_json_delta',partial_json:JSON.stringify(tool)}});
  } else {
    emit(res,'content_block_start',{type:'content_block_start',index:0,content_block:{type:'text',text:''}});
    emit(res,'content_block_delta',{type:'content_block_delta',index:0,delta:{type:'text_delta',text}});
  }
  emit(res,'content_block_stop',{type:'content_block_stop',index:0});
  emit(res,'message_delta',{type:'message_delta',delta:{stop_reason:tool?'tool_use':'end_turn',stop_sequence:null},usage:{output_tokens:1}});
  emit(res,'message_stop',{type:'message_stop'});res.end();
}
function emit(res,event,data){res.write('event: '+event+'\ndata: '+JSON.stringify(data)+'\n\n');}
function reject(res,status,message){res.writeHead(status,{'content-type':'application/json'});res.end(JSON.stringify({type:'error',error:{type:'invalid_request_error',message}}));}

async function run(name,{original=false,gzip='1',mode='normal',tools=false}={}) {
  const directory=path.join(sandboxRoot,name);fs.mkdirSync(directory,{recursive:true});
  const toolInput={file_path:path.join(directory,'fixture.txt'),offset:1,limit:2};
  fs.writeFileSync(toolInput.file_path,'SYNTHETIC_TOOL_FIXTURE\nsecond line\n');
  const received=[],lines=[],timers=new Set();let child,closeObserved=false;
  const server=http.createServer(async(req,res)=>{
    res.on('close',()=>{if(!res.writableEnded)closeObserved=true;});
    const chunks=[];for await(const chunk of req)chunks.push(chunk);
    const wire=Buffer.concat(chunks),encoding=req.headers['content-encoding']||'identity';
    const decoded=encoding==='gzip'?zlib.gunzipSync(wire):wire;
    const body=JSON.parse(decoded);
    received.push({path:req.url,encoding,wireBytes:wire.length,decodedBytes:decoded.length,decoded,body});
    if(req.url.includes('count_tokens')){res.setHeader('content-type','application/json');res.end('{"input_tokens":20}');return;}
    if(mode==='unauthorized'){reject(res,401,'synthetic rejection');return;}
    if(mode==='forbidden'){reject(res,403,'synthetic forbidden');return;}
    if(mode==='invalid'){reject(res,400,'synthetic validation error');return;}
    if(mode==='gzip-rejection'&&encoding==='gzip'){reject(res,400,'The request body is not valid JSON');return;}
    if(mode==='unsupported-encoding'&&encoding==='gzip'){reject(res,415,'synthetic unsupported encoding');return;}
    if(mode==='rate-limit'){reject(res,429,'synthetic rate limit');return;}
    if(mode==='upstream-failure'){reject(res,503,'synthetic upstream failure');return;}
    if(mode==='stream'||mode==='cancel'){
      res.setHeader('content-type','text/event-stream');
      emit(res,'message_start',{type:'message_start',message:{id:'msg_stream_test',type:'message',role:'assistant',model:'claude-sonnet-4-6',content:[],stop_reason:null,stop_sequence:null,usage:{input_tokens:20,output_tokens:1}}});
      emit(res,'content_block_start',{type:'content_block_start',index:0,content_block:{type:'text',text:''}});
      emit(res,'content_block_delta',{type:'content_block_delta',index:0,delta:{type:'text_delta',text:'first'}});
      const timer=setTimeout(()=>{
        timers.delete(timer);if(res.destroyed)return;
        emit(res,'content_block_delta',{type:'content_block_delta',index:0,delta:{type:'text_delta',text:' second'}});
        emit(res,'content_block_stop',{type:'content_block_stop',index:0});
        emit(res,'message_delta',{type:'message_delta',delta:{stop_reason:'end_turn',stop_sequence:null},usage:{output_tokens:2}});
        emit(res,'message_stop',{type:'message_stop'});res.end();
      },mode==='cancel'?10000:500);timers.add(timer);return;
    }
    if(tools&&received.length===1){events(res,{tool:toolInput});return;}
    events(res);
  });
  await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
  const started=Date.now();let stdout='',stderr='',lineBuffer='';
  const args=['-p','--model','claude-sonnet-4-6','--tools',tools?'Read':'','--no-session-persistence','--max-turns','3','--setting-sources','','--disable-slash-commands'];
  if(tools)args.push('--allowedTools','Read');
  if(mode==='stream'||mode==='cancel')args.push('--output-format','stream-json','--verbose','--include-partial-messages');
  else args.push('--output-format','json');
  const env=environment(directory,{ANTHROPIC_BASE_URL:'http://127.0.0.1:'+server.address().port,CLAUDE_CODE_GZIP_REQUEST_BODIES:gzip,CLAUDE_CODE_MAX_RETRIES:'0'});
  try {
    child=spawn(original?binary:prepared.binary,args,{cwd:directory,env,windowsHide:true,stdio:['pipe','pipe','pipe']});
  } catch (error) {
    server.closeAllConnections();await new Promise(resolve=>server.close(resolve));throw error;
  }
  child.stdin.on('error',()=>{});
  child.stdin.end(prompt);
  child.stdout.on('data',chunk=>{
    stdout+=chunk;lineBuffer+=chunk;
    let end;while((end=lineBuffer.indexOf('\n'))>=0){
      const text=lineBuffer.slice(0,end);lineBuffer=lineBuffer.slice(end+1);
      try{const line=JSON.parse(text);lines.push({time:Date.now()-started,line});if(mode==='cancel'&&line.type==='stream_event'&&line.event?.type==='content_block_delta')child.kill();}catch{}
    }
  });
  child.stderr.on('data',chunk=>stderr+=chunk);
  let code;
  const deadline=setTimeout(()=>child.kill(),35000);
  try{code=await new Promise((resolve,reject)=>{child.on('error',reject);child.on('close',resolve);});}
  finally{
    clearTimeout(deadline);for(const timer of timers)clearTimeout(timer);
    await new Promise(resolve=>setTimeout(resolve,80));server.closeAllConnections();await new Promise(resolve=>server.close(resolve));
  }
  let result;try{result=JSON.parse(stdout);}catch{}
  reports.push({name,code,requests:received.map(r=>({encoding:r.encoding,wireBytes:r.wireBytes,decodedBytes:r.decodedBytes})),elapsedMs:Date.now()-started});
  return {code,received,result,stderr,lines,closeObserved,toolInput};
}

before(()=>{originalHash=sha256(fs.readFileSync(binary));prepared=prepareCopy({binary});});
after(()=>{
  assert.equal(sha256(fs.readFileSync(binary)),originalHash,'installed CC was modified');
  fs.writeFileSync(path.join(__dirname,'../test-results.json'),JSON.stringify({version:prepared.receipt.version,sourceHash:originalHash,changedBytes:prepared.receipt.changedBytes,syntheticOnly:true,sourceUnchanged:true,reports},null,2));
});

test('installed CC does not gzip a custom endpoint even with the native flag',async()=>{
  const r=await run('original',{original:true});assert.equal(r.code,0);assert.equal(r.result?.result,'ok');
  assert.equal(r.received.length,1);assert.equal(r.received[0].encoding,'identity');
});
test('verified copy gzips a native request and retains the synthetic Unicode prompt',async()=>{
  const r=await run('gzip');assert.equal(r.code,0);assert.equal(r.result?.result,'ok');assert.equal(r.received.length,1);
  const request=r.received[0];assert.equal(request.encoding,'gzip');assert.ok(request.wireBytes<request.decodedBytes);
  const userContent=request.body.messages.filter(m=>m.role==='user').flatMap(m=>typeof m.content==='string'?[m.content]:m.content.filter(b=>b.type==='text').map(b=>b.text));
  assert.ok(userContent.some(text=>text.includes(prompt)),'the entire prompt, including whitespace/code/Unicode, must survive unchanged');
});
test('native off switch still disables gzip',async()=>{
  const r=await run('off',{gzip:'0'});assert.equal(r.code,0);assert.equal(r.received.length,1);assert.equal(r.received[0].encoding,'identity');
});
test('gzip preserves a tool call input and the next tool result',async()=>{
  const r=await run('tool',{tools:true});assert.equal(r.code,0);assert.equal(r.result?.result,'ok');assert.equal(r.received.length,2);
  assert.ok(r.received.every(x=>x.encoding==='gzip'));
  const blocks=r.received[1].body.messages.flatMap(m=>Array.isArray(m.content)?m.content:[]);
  const call=blocks.find(b=>b.type==='tool_use');assert.deepEqual(call.input,r.toolInput);
  const output=blocks.find(b=>b.type==='tool_result');assert.equal(output.tool_use_id,'toolu_synthetic');
  assert.ok(JSON.stringify(output.content).includes('SYNTHETIC_TOOL_FIXTURE'));
});
test('partial SSE text is delivered before stream completion',async()=>{
  const r=await run('stream',{mode:'stream'});assert.equal(r.code,0);
  const deltas=r.lines.filter(x=>x.line.type==='stream_event'&&x.line.event?.type==='content_block_delta');
  assert.equal(deltas.length,2);assert.equal(deltas[0].line.event.delta.text,'first');
  assert.ok(deltas[1].time-deltas[0].time>=250,'stream increments were buffered together');
});
test('cancellation closes the active request without replay',async()=>{
  const r=await run('cancel',{mode:'cancel'});assert.equal(r.received.length,1);assert.equal(r.closeObserved,true);
});
for(const mode of ['unauthorized','forbidden','invalid','rate-limit','upstream-failure'])test(mode+' retains the original client request count',async()=>{
  const baseline=await run(mode+'-baseline',{mode,original:true});
  const r=await run(mode,{mode});assert.notEqual(r.code,0);assert.equal(r.received.length,baseline.received.length);assert.equal(r.received[0].encoding,'gzip');
});
for (const mode of ['gzip-rejection','unsupported-encoding'])test(mode+' fallback preserves all JSON fields and user text',async()=>{
  const r=await run(mode,{mode});assert.equal(r.code,0);assert.equal(r.result?.result,'ok');
  assert.deepEqual(r.received.map(x=>x.encoding),['gzip','identity']);
  // The built-in identity serializer may add/remove JSON whitespace on fallback.
  // Compare every parsed field; string values (including code) stay exact.
  assert.deepEqual(r.received[0].body,r.received[1].body);
});
