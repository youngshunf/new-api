# Vidu JavaScript 覆盖验证

本目录用标准 Istanbul 对 Vidu 原 JavaScript 建立语句、分支、函数和行覆盖证据。Node 只负责 AST 插装与报告；业务 hook 始终由原 Go 测试及 Sobek Engine 执行，不把 Go 覆盖率当作 JavaScript 覆盖率，不另写一套 Node 业务断言。

默认 Go 测试不需要安装本目录依赖，也不启用覆盖。只有显式设置 `HASN_VIDU_COVERAGE_MANIFEST` 才读取插装制品；缺失文件、源码摘要不符、计数不完整均失败，不回退原源码掩盖问题。此专项不证明真实供应商调用、全部渠道、视频模式或 E2E 已就绪。

## 依赖与本轮目录

需要支持 `structuredClone` 的 Node.js（至少 18）、支持锁文件格式 9 的 pnpm、满足仓内 go.mod 的原生 Go。固定工具为 `istanbul-lib-instrument@6.0.3`、`istanbul-lib-coverage@3.2.2`，传递依赖由本目录 `pnpm-lock.yaml` 锁定。

以下 PowerShell 示例从 NewAPI 仓根准备参数。`$run`、`$report` 必须改为当前任务临时根下的本轮全新绝对路径；其父目录应已存在，禁止指向源码目录、旧证据目录或他人的目录。不要把占位值直接执行。

```powershell
$repo = (Get-Location).Path
$tools = Join-Path $repo 'tests/plugins/coverage'
$run = '<任务临时根>/vidu-js-coverage-<本轮唯一名>'
$report = '<任务临时根>/vidu-js-report-<本轮唯一名>'
$base = '<本次审定的完整Git基线SHA>'
$go = '<已批准的原生Go工具链>/bin/go.exe'
```

先按锁文件安装，仅使用 pnpm，禁止重解锁或启用安装脚本。依赖留在本目录 node_modules，由局部 `.gitignore` 排除；不提交 node_modules、store 或临时制品，不修改 web 或其它仓的锁文件。

```powershell
pnpm --dir "$tools" install --frozen-lockfile --ignore-scripts
if ($LASTEXITCODE -ne 0) { throw '标准覆盖工具安装失败' }
```

## 插装与原 Go 测试

```powershell
node "$tools/instrument.mjs" "$repo/plugins/tasks/vidu/plugin.js" "$run"
if ($LASTEXITCODE -ne 0) { throw '插装失败，不执行覆盖测试' }
```

插装器先处理原 ESM，指定 `globalThis`，禁用通过 Function 查找全局对象；之后才追加未插装的测试桥，桥不进入原源码覆盖分母。桥分别保存原 `buildSubmitRequest` 与 `protocols.openai_video.decodeRequest`，在同一次调用的 finally 中经同步 `Options.Log` 回调发送 runtime 标识、hook 标记及计数，不发送 ctx、请求体、凭据、媒体或返回产物。测试 facade 的 `Call` 与 `CallPath` 只跟踪这两个目标，标记不匹配即失败；默认模式仍直接调用原 Engine。原返回值、普通 JS 异常保持原语义，不重复执行 hook。

以下只列 argv 与环境的 **JSON 数据示例，不是可直接执行的 Go 命令**。将占位值替换为前面确定的绝对路径后，交给原本机公共 support 执行者，复用已有资源登记、Windows Job、预算和排空收束；本目录不提供全自动进程入口，也不创建私有 runner。Windows 不得绕过公共入口裸起进程，不得因资源拒绝自动重试或提高预算。只运行原四项与新增旧入口拒绝用例共五项测试，不扩大到全部包，不起服务、不调用付费上游。

```json
{
  "argv": [
    "<已批准的原生Go工具链>/bin/go.exe", "-C", "<NewAPI仓根绝对路径>",
    "test", "-mod=readonly", "-p=1", "./tests/plugins",
    "-run", "^(TestViduCanonicalVideo(Hook|Modes|RejectsInvalidRoles)|TestViduLegacyVideoHookUnchanged|TestViduLegacyEndpointRejectsCanonicalFields)$",
    "-count=1", "-v"
  ],
  "env": {
    "HASN_VIDU_COVERAGE_MANIFEST": "<本轮run绝对路径>/manifest.json",
    "GOMAXPROCS": "1", "CGO_ENABLED": "0", "GOTOOLCHAIN": "local",
    "GOWORK": "off", "GOENV": "off"
  }
}
```

执行者须记录真实退出码、完整日志、输入摘要与公共资源收束回执；失败不得解释成覆盖通过。覆盖环境只传给该受控子进程，不改全局配置。

每个顶层测试独占一个 raw JSON；每次目标调用须恰好产生一次同步快照。同 runtime 的快照是累计值，collector 只保留最后一份，不重复相加；不同 runtime 再由标准库合并。Cleanup 只保存已收数据，不重新从 runtime 池借实例。中断、取消、缺快照或测试失败均为覆盖不完整，不能报告阈值通过。

## 固定原 Git diff 与报告

源码和基线须在插装、测试、报告期间保持不变。新增范围来自审定基线到本次源码的原 Git diff，不得换成测试文件行或只挑已命中行。补丁按 Git stdout 原字节保存，不经过 PowerShell 文本重定向、编码或换行转换。

```powershell
$patch = Join-Path $run 'source.diff'
$diffCapture = @'
import pathlib, subprocess, sys
repo, baseline, target = sys.argv[1:]
data = subprocess.check_output(['git', '-C', repo, 'diff', '--no-ext-diff', '--no-color', '--unified=0', baseline, '--', 'plugins/tasks/vidu/plugin.js'])
with pathlib.Path(target).open('xb') as output:
    output.write(data)
'@
& python -I -c $diffCapture "$repo" "$base" "$patch"
if ($LASTEXITCODE -ne 0) { throw '原Git差异读取失败或证据文件已存在' }
node "$tools/report.mjs" "$run/manifest.json" "$patch" "$report"
if ($LASTEXITCODE -ne 0) { throw '覆盖报告缺损或输入不一致' }
```

报告必须有五份完整 raw：`TestViduCanonicalVideoHook.json`、`TestViduCanonicalVideoModes.json`、`TestViduCanonicalVideoRejectsInvalidRoles.json`、`TestViduLegacyVideoHookUnchanged.json`、`TestViduLegacyEndpointRejectsCanonicalFields.json`，并核对原源 SHA、插装 SHA、固定工具版本、原位置映射、调用与事件数。空 diff、缺 raw、`complete=false`、摘要不符均拒绝。输出保留：

- `raw-evidence.json`：五份原始最终累计快照及证据元信息；
- `coverage-full.json`：完整原文件合并计数；
- `coverage-changed.json`：新增范围所涉及的完整计数项；
- `summary.json`：全文件与新增范围各自的标准统计、全部未命中语句和分支；
- `source.diff`：本轮固定差异。

新增范围按原源码新增行与 statementMap、branchMap 等位置相交选择；某分支组任一位置相交，保留该组全部出口，不能只选命中出口。因此 `changed_source` 是“新增范围涉及的计数项”，不是全文件覆盖率，也不等同于按每条新增文本行直接计数。`full_file` 始终保留未触达的旧代码，不以增量结果冒充整文件 80%。

**report 返回 0 只表示制品完整且报告生成成功，不是 80% 阈值门。必须读取实际分子、分母、百分比和未命中项，再按本次验收范围判断。**

```powershell
$summary = Get-Content -LiteralPath "$report/summary.json" -Raw -Encoding UTF8 | ConvertFrom-Json
$summary.changed_source.summary | Format-List
$summary.full_file.summary | Format-List
$summary.changed_source.uncovered | ConvertTo-Json -Depth 20
```

保留 manifest、initial、插装副本、raw、固定 diff、报告与公共执行回执。原件不可覆盖；反证只在新独立副本中修改。不要把包含敏感信息的文件当输入，不读取或分发秘密目录。
