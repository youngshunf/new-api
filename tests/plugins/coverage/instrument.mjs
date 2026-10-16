import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

// 只从本脚本所在的已锁工具目录加载标准插装器，不执行输入源码。
const require = createRequire(import.meta.url);
const { createInstrumenter } = require('istanbul-lib-instrument');
const versions = {
  'istanbul-lib-instrument': require('istanbul-lib-instrument/package.json').version,
  'istanbul-lib-coverage': require('istanbul-lib-coverage/package.json').version,
};
assert.deepEqual(versions, { 'istanbul-lib-instrument': '6.0.3', 'istanbul-lib-coverage': '3.2.2' });
const [sourceArg, outputArg, ...extra] = process.argv.slice(2);
assert(sourceArg && outputArg && extra.length === 0, '用法：node instrument.mjs <原plugin.js> <全新输出目录>');
const sourceFile = fs.realpathSync(sourceArg);
const output = path.resolve(outputArg);
assert(!fs.existsSync(output), '拒绝覆盖已有插装轮次');
const original = fs.readFileSync(sourceFile, 'utf8');
const digest = value => crypto.createHash('sha256').update(value).digest('hex');
const instrumenter = createInstrumenter({
  esModules: true,
  coverageVariable: '__coverage__',
  coverageGlobalScope: 'globalThis',
  coverageGlobalScopeFunc: false,
  produceSourceMap: false,
  compact: false,
  preserveComments: true,
});
const instrumented = instrumenter.instrumentSync(original, sourceFile);
const initial = instrumenter.lastFileCoverage();
assert(initial && Object.keys(initial.statementMap).length > 0, '未产生真实语句映射');

// 原源码已完成标准AST插装；以下测试桥不计入原源码覆盖分母。
// finally只序列化计数，不携带ctx、请求参数、凭据或返回产物。
const bridge = `
const __hasnViduCoverageRuntime = utils.uuid();
const __hasnViduOriginalBuildSubmit = buildSubmitRequest;
buildSubmitRequest = function (...args) {
  try {
    return __hasnViduOriginalBuildSubmit.apply(this, args);
  } finally {
    console.log("HASN_VIDU_COVERAGE:" + JSON.stringify({
      runtime_id: __hasnViduCoverageRuntime,
      hook: "buildSubmitRequest",
      coverage: globalThis.__coverage__
    }));
  }
};
const __hasnViduOriginalVideoDecode = protocols.openai_video.decodeRequest;
protocols.openai_video.decodeRequest = function (...args) {
  try {
    return __hasnViduOriginalVideoDecode.apply(this, args);
  } finally {
    console.log("HASN_VIDU_COVERAGE:" + JSON.stringify({
      runtime_id: __hasnViduCoverageRuntime,
      hook: "protocols.openai_video.decodeRequest",
      coverage: globalThis.__coverage__
    }));
  }
};
`;
const result = instrumented + '\n' + bridge;
fs.mkdirSync(output);
fs.mkdirSync(path.join(output, 'raw'));
const instrumentedFile = path.join(output, 'plugin.coverage.js');
fs.writeFileSync(instrumentedFile, result, { flag: 'wx' });
fs.writeFileSync(path.join(output, 'initial.json'), JSON.stringify(initial, null, 2), { flag: 'wx' });
const manifest = {
  source_file: sourceFile,
  source_sha256: digest(original),
  instrumented_file: instrumentedFile,
  instrumented_sha256: digest(result),
  tool_versions: versions,
  output_directory: path.join(output, 'raw'),
};
fs.writeFileSync(path.join(output, 'manifest.json'), JSON.stringify(manifest, null, 2), { flag: 'wx' });
console.log(path.join(output, 'manifest.json'));
