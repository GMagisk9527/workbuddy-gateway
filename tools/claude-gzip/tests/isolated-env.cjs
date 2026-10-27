'use strict';
const fs = require('node:fs');
const path = require('node:path');
exports.binary = path.join(process.env.APPDATA, 'npm/node_modules/@anthropic-ai/claude-code/bin/claude.exe');
exports.environment = function environment(sandbox, extra = {}) {
  fs.mkdirSync(sandbox, {recursive: true});
  const env = {};
  for (const name of ['SystemRoot', 'WINDIR', 'PATH', 'TEMP', 'TMP', 'COMSPEC', 'PATHEXT', 'APPDATA', 'LOCALAPPDATA', 'USERPROFILE']) {
    if (process.env[name]) env[name] = process.env[name];
  }
  return Object.assign(env, {
    CLAUDE_CONFIG_DIR: path.join(sandbox, 'profile'),
    DISABLE_TELEMETRY: '1', DISABLE_ERROR_REPORTING: '1', DISABLE_AUTOUPDATER: '1',
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
    ANTHROPIC_BASE_URL: 'http://127.0.0.1:1',
    ANTHROPIC_AUTH_TOKEN: 'synthetic-local-test-only',
    NO_PROXY: '*',
  }, extra);
};
