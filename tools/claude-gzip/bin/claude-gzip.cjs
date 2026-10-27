#!/usr/bin/env node
'use strict';
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {prepareCopy, defaultBinary, validateGateway, DEFAULT_GATEWAY} = require('../lib/native-patch.cjs');

async function main() {
  const args = process.argv.slice(2);
  let binary, prepareOnly = false, original = false;
  while (args.length && args[0] !== '--') {
    if (args[0] === '--prepare-only') {prepareOnly = true; args.shift();}
    else if (args[0] === '--original') {original = true; args.shift();}
    else if (args[0] === '--binary') {args.shift(); binary = args.shift(); if (!binary) throw new Error('--binary requires a path');}
    else break;
  }
  if (args[0] === '--') args.shift();
  if (prepareOnly && original) throw new Error('--prepare-only and --original cannot be combined');
  // Rollback must remain usable after an official client update changes its hash.
  if (original) {
    run(fs.realpathSync(binary || defaultBinary()), args, {...process.env});
    return;
  }
  const prepared = prepareCopy(binary ? {binary} : {});
  if (prepareOnly) {
    console.log(JSON.stringify({version: prepared.receipt.version, binary: prepared.binary, cached: prepared.cached, changedBytes: prepared.receipt.changedBytes, sourceUnchanged: true}));
    return;
  }
  const env = {...process.env};
  {
    validateGateway(env.ANTHROPIC_BASE_URL || DEFAULT_GATEWAY);
    // An override settings layer sets only public transport configuration. CC
    // continues to load its own credentials; this program never reads them.
    env.ANTHROPIC_BASE_URL = DEFAULT_GATEWAY;
    env.CLAUDE_CODE_GZIP_REQUEST_BODIES = '1';
    env.DISABLE_AUTOUPDATER = '1';
    for (const key of Object.keys(env)) if (/^(?:https?_proxy|all_proxy)$/i.test(key)) delete env[key];
    const transportSettings = path.join(__dirname, '../direct-settings.json');
    if (!fs.existsSync(transportSettings)) throw new Error('Missing direct transport settings');
    args.unshift('--settings', transportSettings);
    process.stderr.write('[cc-gzip] gzip enabled; HTTPS directly to WorkBuddy; original CC unchanged.\n');
  }
  run(prepared.binary, args, env);
}

function run(binary, args, env) {
  // Console Ctrl+C also reaches the native child. Keep the launcher alive so
  // CC can handle its normal interrupt / cancel behavior instead of orphaning it.
  const onInterrupt = () => {};
  process.on('SIGINT', onInterrupt);
  const child = spawn(binary, args, {env, stdio: 'inherit', windowsHide: false});
  child.on('error', error => {process.removeListener('SIGINT', onInterrupt); console.error('[cc-gzip] ' + error.message); process.exitCode = 1;});
  child.on('exit', (code, signal) => {process.removeListener('SIGINT', onInterrupt); process.exitCode = code ?? (signal ? 130 : 1);});
}
main().catch(error => {console.error('[cc-gzip] ' + error.message); process.exitCode = 1;});
