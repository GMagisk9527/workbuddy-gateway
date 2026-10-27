'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const {spawnSync} = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {environment} = require('./isolated-env.cjs');
const launcher = path.resolve(__dirname,'../bin/claude-gzip.cjs');

test('rollback runs the original program without preparing or hashing a copy', t => {
  const sandbox = fs.mkdtempSync(path.join(os.tmpdir(),'cc-gzip-launch-'));
  t.after(()=>fs.rmSync(sandbox,{recursive:true}));
  // This source is deliberately outside the patch allowlist. It can still run
  // through --original, just as a newly updated official client must be able to.
  const script = path.join(sandbox,'echo.cjs');
  fs.writeFileSync(script,'console.log(JSON.stringify({args:process.argv.slice(2),gzip:process.env.CLAUDE_CODE_GZIP_REQUEST_BODIES}));');
  const env = environment(sandbox,{CLAUDE_CODE_GZIP_REQUEST_BODIES:'0'});
  const r = spawnSync(process.execPath,[launcher,'--original','--binary',process.execPath,'--',script,'argument with spaces'],{env,encoding:'utf8',windowsHide:true,timeout:10000});
  assert.equal(r.status,0,r.stderr);
  assert.deepEqual(JSON.parse(r.stdout),{args:['argument with spaces'],gzip:'0'});
});

test('unknown source fails before an attempted launch', () => {
  const r = spawnSync(process.execPath,[launcher,'--binary',process.execPath,'--','--version'],{env:environment(path.join(__dirname,'.sandbox/unsupported-launch')),encoding:'utf8',windowsHide:true,timeout:10000});
  assert.notEqual(r.status,0);
  assert.match(r.stderr,/has not been verified/);
  assert.equal(r.stdout,'');
});
