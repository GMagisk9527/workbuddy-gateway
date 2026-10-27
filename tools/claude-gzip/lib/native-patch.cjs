'use strict';
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');

const VERSION = '2.1.311';
// A fingerprint is added only after local mock-server integration checks pass.
const VERIFIED_SOURCES = new Set(['d3fe4adaf1918347e76824e01e199a6cd7387e07c0e787ed7af758d21cf2ef42']);
const ANCHOR = Buffer.from('function aGt(e,r,s,n="api"){if(!Tc(e))return;let d=mZn(r),l=w(n);');
const REPLACEMENT = Buffer.from('function aGt(e,r,s,n="api"){if(!w(n)) return;let d=mZn(r),l=w(n);');
// A generic gateway 400/403 is not evidence that gzip was rejected. Only a 415
// or the client's existing explicit invalid-JSON classifier permits fallback.
const REJECTION_ANCHOR = Buffer.from('if(!Q(e))return"non_anthropic_rejection";');
const REJECTION_REPLACEMENT = Buffer.from('if(!Q(e)&&e.status===415)return"gzip";'.padEnd(REJECTION_ANCHOR.length));
const PATCH_REVISION = 2;
const TRAILER = Buffer.from('\n---- Bun! ----\n');
const DEFAULT_GATEWAY = 'https://fandai.202695276.xyz:10717';
const sha256 = buffer => crypto.createHash('sha256').update(buffer).digest('hex');

function checkedRange(buffer, offset, length, label) {
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(length) || offset < 0 || length < 0 || offset > buffer.length - length) {
    throw new Error('Invalid executable range: ' + label);
  }
  return buffer.subarray(offset, offset + length);
}

function moduleForOffset(buffer, sourceOffset, sourceLength) {
  if (buffer.length < 64 || buffer.toString('ascii', 0, 2) !== 'MZ') throw new Error('Only verified Windows PE executables are supported');
  const pe = buffer.readUInt32LE(0x3c);
  checkedRange(buffer, pe, 24, 'PE header');
  if (buffer.toString('binary', pe, pe + 4) !== 'PE\0\0') throw new Error('Invalid PE signature');
  if (buffer.readUInt16LE(pe + 4) !== 0x8664) throw new Error('Only Windows x64 is supported');
  const sections = buffer.readUInt16LE(pe + 6);
  const table = pe + 24 + buffer.readUInt16LE(pe + 20);
  checkedRange(buffer, table, sections * 40, 'section table');
  const matches = [];
  for (let i = 0; i < sections; i++) {
    const section = table + i * 40;
    if (buffer.toString('ascii', section, section + 8).replace(/\0.*$/, '') !== '.bun') continue;
    const rawStart = buffer.readUInt32LE(section + 20);
    const rawLength = buffer.readUInt32LE(section + 16);
    checkedRange(buffer, rawStart, rawLength, 'Bun section');
    checkedRange(buffer, rawStart, 8, 'Bun size');
    const size = Number(buffer.readBigUInt64LE(rawStart));
    if (!Number.isSafeInteger(size) || size < TRAILER.length + 32 || size > rawLength - 8) throw new Error('Unsupported Bun section format');
    const base = rawStart + 8;
    const trailer = checkedRange(buffer, base + size - TRAILER.length, TRAILER.length, 'Bun trailer');
    if (!trailer.equals(TRAILER)) throw new Error('Unsupported Bun trailer');
    const offsets = base + size - TRAILER.length - 32;
    if (buffer.readBigUInt64LE(offsets) !== BigInt(offsets - base)) throw new Error('Unsupported Bun offsets');
    const moduleStart = buffer.readUInt32LE(offsets + 8);
    const moduleLength = buffer.readUInt32LE(offsets + 12);
    if (moduleLength === 0 || moduleLength % 52 !== 0 || moduleStart + moduleLength > offsets - base) throw new Error('Unsupported Bun module table');
    for (let p = base + moduleStart; p < base + moduleStart + moduleLength; p += 52) {
      const start = base + buffer.readUInt32LE(p + 8);
      const length = buffer.readUInt32LE(p + 12);
      if (start <= sourceOffset && sourceOffset + sourceLength <= start + length) {
        checkedRange(buffer, start, length, 'module source');
        if (start + length > offsets) throw new Error('Module source overlaps Bun metadata');
        const nameStart = base + buffer.readUInt32LE(p);
        const nameLength = buffer.readUInt32LE(p + 4);
        const name = checkedRange(buffer, nameStart, nameLength, 'module name').toString('utf8');
        const bytecodeOffset = buffer.readUInt32LE(p + 24);
        const bytecodeLength = buffer.readUInt32LE(p + 28);
        if (bytecodeLength === 0 || bytecodeOffset + bytecodeLength > offsets - base) throw new Error('Unexpected gzip bytecode layout');
        matches.push({name, pointer: p + 24, sourceStart: start, sourceLength: length});
      }
    }
  }
  if (matches.length !== 1) throw new Error('Expected exactly one gzip module');
  return matches[0];
}

function patchBuffer(original) {
  if (ANCHOR.length !== REPLACEMENT.length) throw new Error('Internal patch size mismatch');
  const offset = original.indexOf(ANCHOR);
  if (offset < 0 || original.indexOf(ANCHOR, offset + ANCHOR.length) >= 0) throw new Error('Unsupported gzip source; original executable was not changed');
  const module = moduleForOffset(original, offset, ANCHOR.length);
  const rejectionOffset = original.indexOf(REJECTION_ANCHOR, module.sourceStart);
  if (rejectionOffset < module.sourceStart || rejectionOffset + REJECTION_ANCHOR.length > module.sourceStart + module.sourceLength ||
      original.indexOf(REJECTION_ANCHOR, rejectionOffset + 1) >= 0) throw new Error('Unsupported gzip rejection source');
  const patched = Buffer.from(original);
  REPLACEMENT.copy(patched, offset);
  REJECTION_REPLACEMENT.copy(patched, rejectionOffset);
  // Reparse just this module from its complete embedded source. Its old cached
  // bytecode otherwise executes the old predicate. All other modules stay as-is.
  patched.fill(0, module.pointer, module.pointer + 8);
  let changedBytes = 0;
  for (let i = 0; i < original.length; i++) if (original[i] !== patched[i]) changedBytes++;
  return {patched, module: module.name, sourceOffset: offset, rejectionOffset, bytecodePointerOffset: module.pointer, changedBytes};
}

function defaultBinary() {
  if (process.platform !== 'win32' || !process.env.APPDATA) throw new Error('Specify a verified Windows CC binary with --binary');
  return path.join(process.env.APPDATA, 'npm/node_modules/@anthropic-ai/claude-code/bin/claude.exe');
}

function prepareCopy({binary = defaultBinary(), cacheRoot = path.join(__dirname, '../.cache')} = {}) {
  const source = fs.realpathSync(binary);
  const original = fs.readFileSync(source);
  const sourceHash = sha256(original);
  if (!VERIFIED_SOURCES.has(sourceHash)) throw new Error('This CC executable has not been verified. No file was changed. Supported version: ' + VERSION);
  const directory = path.resolve(cacheRoot, sourceHash, 'revision-' + PATCH_REVISION);
  const target = path.join(directory, 'claude.exe');
  const receiptPath = path.join(directory, 'receipt.json');
  const result = patchBuffer(original);
  const expectedHash = sha256(result.patched);
  if (fs.existsSync(target) || fs.existsSync(receiptPath)) {
    if (!fs.existsSync(target) || !fs.existsSync(receiptPath)) throw new Error('Incomplete gzip copy; no executable was started');
    const receipt = JSON.parse(fs.readFileSync(receiptPath, 'utf8'));
    if (receipt.sourceHash !== sourceHash || receipt.patchRevision !== PATCH_REVISION || receipt.patchedHash !== expectedHash ||
        sha256(fs.readFileSync(target)) !== expectedHash) throw new Error('Gzip copy fingerprint mismatch; no executable was started');
    return {binary: target, source, receipt, cached: true};
  }
  const receipt = {version: VERSION, patchRevision: PATCH_REVISION, sourceHash, patchedHash: expectedHash, module: result.module, sourceOffset: result.sourceOffset, rejectionOffset: result.rejectionOffset, bytecodePointerOffset: result.bytecodePointerOffset, changedBytes: result.changedBytes};
  fs.mkdirSync(directory, {recursive: true});
  const temporary = target + '.' + crypto.randomUUID() + '.tmp';
  try {
    fs.writeFileSync(temporary, result.patched, {flag: 'wx'});
    if (sha256(fs.readFileSync(source)) !== sourceHash) throw new Error('CC changed during preparation; retry after the update finishes');
    if (fs.existsSync(target) || fs.existsSync(receiptPath)) throw new Error('Another preparation completed concurrently; retry');
    fs.renameSync(temporary, target);
    fs.writeFileSync(receiptPath, JSON.stringify(receipt, null, 2) + '\n', {flag: 'wx'});
  } finally {
    if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
  }
  return {binary: target, source, receipt, cached: false};
}

function validateGateway(input = DEFAULT_GATEWAY) {
  const url = new URL(input);
  if (url.origin !== DEFAULT_GATEWAY || url.username || url.password || (url.pathname !== '/' && url.pathname !== '') || url.search || url.hash) {
    throw new Error('This launcher is restricted to the verified direct WorkBuddy gateway');
  }
  return url.origin;
}

module.exports = {VERSION, ANCHOR, REPLACEMENT, REJECTION_ANCHOR, REJECTION_REPLACEMENT, DEFAULT_GATEWAY, sha256, moduleForOffset, patchBuffer, prepareCopy, defaultBinary, validateGateway};
