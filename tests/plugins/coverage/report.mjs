import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';

// 覆盖语义、合并及百分比全部由已锁标准库提供，Node不执行任何plugin行为。
const require = createRequire(import.meta.url);
const { createCoverageMap, createFileCoverage } = require('istanbul-lib-coverage');
const versions = { 'istanbul-lib-instrument': require('istanbul-lib-instrument/package.json').version, 'istanbul-lib-coverage': require('istanbul-lib-coverage/package.json').version };
assert.deepEqual(versions, { 'istanbul-lib-instrument': '6.0.3', 'istanbul-lib-coverage': '3.2.2' });
const [manifestArg, diffArg, outputArg, ...extra] = process.argv.slice(2);
assert(manifestArg && diffArg && outputArg && extra.length === 0, '用法：node report.mjs <manifest.json> <原Git diff -U0文件> <全新报告目录>');
const readJSON = file => JSON.parse(fs.readFileSync(file, 'utf8'));
const digest = value => crypto.createHash('sha256').update(value).digest('hex');
const manifestPath = fs.realpathSync(manifestArg);
const manifest = readJSON(manifestPath);
assert.deepEqual(manifest.tool_versions, versions);
assert.equal(digest(fs.readFileSync(manifest.source_file)), manifest.source_sha256, '原源码已改变');
assert.equal(digest(fs.readFileSync(manifest.instrumented_file)), manifest.instrumented_sha256, '插装输入已改变');
const initial = readJSON(path.join(path.dirname(manifestPath), 'initial.json'));
assert.equal(initial.path, manifest.source_file);
const coverage = createCoverageMap({});
coverage.addFileCoverage(structuredClone(initial));
const rawFiles = ['TestViduCanonicalVideoHook', 'TestViduCanonicalVideoModes', 'TestViduCanonicalVideoRejectsInvalidRoles', 'TestViduLegacyVideoHookUnchanged', 'TestViduLegacyEndpointRejectsCanonicalFields'].map(name => name + '.json');
const rawEvidence = {};
for (const name of rawFiles) {
  const raw = readJSON(path.join(manifest.output_directory, name));
  assert.equal(raw.test_name + '.json', name);
  assert.equal(raw.complete, true, `${name}覆盖不完整`);
  assert(raw.calls > 0 && raw.calls === raw.events, `${name}调用与同步事件不一致`);
  assert.equal(raw.source_sha256, manifest.source_sha256);
  assert.equal(raw.instrumented_sha256, manifest.instrumented_sha256);
  assert.deepEqual(raw.tool_versions, versions);
  assert(Object.keys(raw.runtimes).length > 0, `${name}缺runtime快照`);
  for (const snapshot of Object.values(raw.runtimes)) {
    const single = createCoverageMap(snapshot);
    assert.deepEqual(single.files(), [initial.path], '不得混入其它脚本计数');
    const data = single.fileCoverageFor(initial.path).toJSON();
    for (const key of ['statementMap', 'branchMap', 'fnMap']) assert.deepEqual(data[key], initial[key], `${name}位置映射变化`);
    coverage.merge(single);
  }
  rawEvidence[name] = raw;
}

// 只消费参数指定的原Git补丁，逐行提取目标文件新增行；不自动选择基线或扫描仓库。
const diff = fs.readFileSync(diffArg, 'utf8');
const changed = new Set();
let selected = false, next = null;
for (const line of diff.split(/\r?\n/)) {
  if (line.startsWith('diff --git ')) { selected = false; next = null; }
  if (line.startsWith('+++ ')) {
    const name = line.slice(4).replace(/^b\//, '');
    selected = manifest.source_file.replaceAll('\\', '/').endsWith('/' + name);
    next = null;
    continue;
  }
  if (!selected) continue;
  const hunk = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(line);
  if (hunk) { next = Number(hunk[1]); continue; }
  if (next === null) continue;
  if (line.startsWith('+')) { changed.add(next); next++; }
  else if (line.startsWith(' ')) next++;
}
assert(changed.size > 0, '补丁没有目标源码新增行，不能将测试行或空范围当覆盖');
const touches = loc => loc && [...changed].some(line => line >= loc.start.line && (line < loc.end.line || (line === loc.end.line && loc.end.column > 0)));
const full = coverage.fileCoverageFor(initial.path).toJSON();
const subset = structuredClone(full);
for (const [mapKey, countKey, include] of [
  ['statementMap', 's', loc => touches(loc)],
  ['fnMap', 'f', loc => touches(loc.loc)],
  // 任一分支位置相交就保留整组分支的所有出口，禁止只挑命中出口。
  ['branchMap', 'b', loc => touches(loc.loc) || loc.locations.some(touches)],
]) {
  for (const [id, loc] of Object.entries(subset[mapKey])) {
    if (!include(loc)) { delete subset[mapKey][id]; delete subset[countKey][id]; }
  }
}
assert(Object.keys(subset.s).length > 0 && Object.keys(subset.b).length > 0, '新增范围无可统计语句或分支');
function uncovered(data) {
  return {
    statements: Object.entries(data.s).filter(([, count]) => count === 0).map(([id]) => ({ id, location: data.statementMap[id] })),
    branches: Object.entries(data.b).flatMap(([id, counts]) => counts.flatMap((count, arm) => count === 0 ? [{ id, arm, location: data.branchMap[id].locations[arm] }] : [])),
  };
}
const report = {
  source_file: manifest.source_file, source_sha256: manifest.source_sha256, instrumented_sha256: manifest.instrumented_sha256,
  tool_versions: versions, diff_sha256: digest(diff), added_lines: [...changed].sort((a, b) => a - b),
  selection: '原Git新增行与原源码位置相交；分支任一位置相交保留整组出口，不以测试行作分母',
  full_file: { summary: coverage.getCoverageSummary().toJSON(), uncovered: uncovered(full) },
  changed_source: { summary: createFileCoverage(subset).toSummary().toJSON(), uncovered: uncovered(subset) },
};
const output = path.resolve(outputArg);
assert(!fs.existsSync(output), '拒绝覆盖已有覆盖报告');
fs.mkdirSync(output);
for (const [name, data] of Object.entries({ 'raw-evidence.json': rawEvidence, 'coverage-full.json': coverage.toJSON(), 'coverage-changed.json': subset, 'summary.json': report })) {
  fs.writeFileSync(path.join(output, name), JSON.stringify(data, null, 2), { flag: 'wx' });
}
fs.writeFileSync(path.join(output, 'source.diff'), diff, { flag: 'wx' });
console.log(JSON.stringify(report, null, 2));
