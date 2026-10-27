'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {ANCHOR, REPLACEMENT, REJECTION_ANCHOR, REJECTION_REPLACEMENT, patchBuffer, moduleForOffset, prepareCopy, validateGateway, DEFAULT_GATEWAY} = require('../lib/native-patch.cjs');

function fixture() {
  const buffer = Buffer.alloc(2048), pe = 128, section = pe + 24 + 240, raw = 1024, base = raw + 8, size = 512;
  buffer.write('MZ');buffer.writeUInt32LE(pe, 0x3c);buffer.write('PE\0\0', pe, 'binary');
  buffer.writeUInt16LE(0x8664, pe + 4);buffer.writeUInt16LE(1, pe + 6);buffer.writeUInt16LE(240, pe + 20);
  buffer.write('.bun', section);buffer.writeUInt32LE(1024, section + 16);buffer.writeUInt32LE(raw, section + 20);
  const source = Buffer.concat([ANCHOR, Buffer.from(' '), REJECTION_ANCHOR]);
  buffer.writeBigUInt64LE(BigInt(size), raw);source.copy(buffer, base + 128);buffer.write('gzip.js', base + 300);
  const p = base + 400;
  for (const [offset, value] of [[0,300],[4,7],[8,128],[12,source.length],[24,32],[28,64]]) buffer.writeUInt32LE(value, p + offset);
  const offsets = base + size - 48;
  buffer.writeBigUInt64LE(464n, offsets);buffer.writeUInt32LE(400, offsets + 8);buffer.writeUInt32LE(52, offsets + 12);
  buffer.write('\n---- Bun! ----\n', base + size - 16);
  return {buffer, offset: base + 128, pointer: p + 24, module: p};
}

test('changes only the opt-in predicate and the selected cached bytecode pointer', () => {
  const {buffer, offset, pointer} = fixture(), before = Buffer.from(buffer);
  const result = patchBuffer(buffer);
  assert.deepEqual(buffer, before, 'input executable was modified');
  assert.equal(result.module, 'gzip.js');
  assert.equal(result.patched.length, buffer.length);
  assert.deepEqual(result.patched.subarray(offset, offset + REPLACEMENT.length), REPLACEMENT);
  assert.deepEqual(result.patched.subarray(result.rejectionOffset, result.rejectionOffset + REJECTION_ANCHOR.length), REJECTION_REPLACEMENT);
  assert.equal(result.patched.readBigUInt64LE(pointer), 0n);
  for (let i = 0; i < buffer.length; i++) {
    if ((i >= offset && i < offset + ANCHOR.length) || (i >= pointer && i < pointer + 8)) continue;
    if (i >= result.rejectionOffset && i < result.rejectionOffset + REJECTION_ANCHOR.length) continue;
    assert.equal(result.patched[i], buffer[i], 'unrelated executable byte changed at ' + i);
  }
});

test('rejects partial / repeated patches and ambiguous source matches', () => {
  const {buffer} = fixture();
  assert.throws(() => patchBuffer(patchBuffer(buffer).patched), /Unsupported gzip source/);
  ANCHOR.copy(buffer, 64);
  assert.throws(() => patchBuffer(buffer), /Unsupported gzip source/);
});

test('rejects invalid PE, module and bytecode layouts', () => {
  assert.throws(() => patchBuffer(Buffer.alloc(1)), /Unsupported/);
  for (const change of [
    b => b.write('XX', 0),
    b => b.writeUInt16LE(0xAA64, 128 + 4),
    b => b.writeUInt32LE(0xFFFFFFFF, 128 + 24 + 240 + 20),
    b => b.writeUInt32LE(0xFFFFFFFF, 1032 + 464 + 8),
    b => b.writeUInt32LE(0, 1032 + 400 + 28),
  ]) {
    const {buffer, offset} = fixture();change(buffer);
    assert.throws(() => moduleForOffset(buffer, offset, ANCHOR.length));
  }
});

test('unknown executable fingerprint is rejected before creating a copy', t => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'cc-gzip-unit-'));
  t.after(() => {for (const name of fs.readdirSync(root)) fs.unlinkSync(path.join(root,name));fs.rmdirSync(root);});
  const binary = path.join(root,'original.exe'), cacheRoot = path.join(root,'cache');
  const {buffer} = fixture();fs.writeFileSync(binary,buffer);
  assert.throws(() => prepareCopy({binary,cacheRoot}), /has not been verified/);
  assert.equal(fs.existsSync(cacheRoot),false);
  assert.deepEqual(fs.readFileSync(binary),buffer);
});

test('launcher permits only the intended HTTPS gateway without credentials or path changes', () => {
  assert.equal(validateGateway(), DEFAULT_GATEWAY);
  assert.equal(validateGateway(DEFAULT_GATEWAY + '/'), DEFAULT_GATEWAY);
  for (const value of ['http://fandai.202695276.xyz:10717','https://fandai.202695276.xyz','https://example.com:10717','https://u:p@fandai.202695276.xyz:10717','https://fandai.202695276.xyz:10717/v1','https://fandai.202695276.xyz:10717?x=1']) {
    assert.throws(() => validateGateway(value));
  }
});
