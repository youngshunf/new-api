package plugins_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/grafana/sobek"
	"github.com/stretchr/testify/require"
)

type viduCoverageManifest struct {
	SourceFile         string            `json:"source_file"`
	SourceSHA256       string            `json:"source_sha256"`
	InstrumentedFile   string            `json:"instrumented_file"`
	InstrumentedSHA256 string            `json:"instrumented_sha256"`
	ToolVersions       map[string]string `json:"tool_versions"`
	OutputDirectory    string            `json:"output_directory"`
}

type viduTestPlugin struct{ Engine *viduTestEngine }
type viduTestEngine struct {
	engine     *jsplugin.Engine
	t          *testing.T
	mu         sync.Mutex
	coverage   bool
	events     int
	calls      int
	incomplete bool
	activeHook string
	runtimes   map[string]json.RawMessage
}

// newViduPluginForTest 默认执行原嵌入源码；覆盖模式仅替换测试输入，不修改生产Engine。
func newViduPluginForTest(t *testing.T) *viduTestPlugin {
	t.Helper()
	source, err := builtinplugins.Source("vidu")
	require.NoError(t, err)
	engine := &viduTestEngine{t: t}
	options := jsplugin.Options{Key: "vidu"}
	if manifestPath := os.Getenv("HASN_VIDU_COVERAGE_MANIFEST"); manifestPath != "" {
		require.True(t, filepath.IsAbs(manifestPath), "覆盖manifest必须是明确绝对路径")
		data, err := os.ReadFile(manifestPath)
		require.NoError(t, err, "覆盖模式缺manifest不得回退原源码")
		var manifest viduCoverageManifest
		require.NoError(t, common.Unmarshal(data, &manifest))
		digest := sha256.Sum256([]byte(source))
		require.Equal(t, hex.EncodeToString(digest[:]), manifest.SourceSHA256, "manifest必须对应本次真实嵌入源码")
		require.True(t, filepath.IsAbs(manifest.SourceFile))
		require.Equal(t, map[string]string{"istanbul-lib-instrument": "6.0.3", "istanbul-lib-coverage": "3.2.2"}, manifest.ToolVersions)
		require.Equal(t, filepath.Join(filepath.Dir(manifestPath), "plugin.coverage.js"), manifest.InstrumentedFile)
		require.Equal(t, filepath.Join(filepath.Dir(manifestPath), "raw"), manifest.OutputDirectory)
		instrumented, err := os.ReadFile(manifest.InstrumentedFile)
		require.NoError(t, err)
		digest = sha256.Sum256(instrumented)
		require.Equal(t, hex.EncodeToString(digest[:]), manifest.InstrumentedSHA256)
		info, err := os.Stat(manifest.OutputDirectory)
		require.NoError(t, err)
		require.True(t, info.IsDir(), "覆盖输出目录必须由本轮插装准备")
		require.NotContains(t, t.Name(), "/", "每个顶层测试独占一个raw文件")
		output := filepath.Join(manifest.OutputDirectory, t.Name()+".json")
		_, err = os.Stat(output)
		require.True(t, os.IsNotExist(err), "拒绝覆盖已有raw证据")
		engine.coverage = true
		engine.runtimes = make(map[string]json.RawMessage)
		options.Log = func(message string) {
			_, payload, found := strings.Cut(message, " HASN_VIDU_COVERAGE:")
			if !found {
				return
			}
			engine.events++
			var event struct {
				RuntimeID string          `json:"runtime_id"`
				Hook      string          `json:"hook"`
				Coverage  json.RawMessage `json:"coverage"`
			}
			if err := common.Unmarshal([]byte(payload), &event); err != nil || event.RuntimeID == "" || event.Hook == "" || event.Hook != engine.activeHook || len(event.Coverage) == 0 || string(event.Coverage) == "null" {
				engine.incomplete = true
				t.Error("覆盖不完整：同步计数事件缺损")
				return
			}
			// 同runtime是累计值，只保留最后一次；不重复相加。
			engine.runtimes[event.RuntimeID] = event.Coverage
		}
		t.Cleanup(func() {
			engine.mu.Lock()
			defer engine.mu.Unlock()
			complete := !engine.incomplete && !t.Failed() && engine.calls > 0 && len(engine.runtimes) > 0
			payload, err := common.Marshal(map[string]any{
				"test_name": t.Name(), "source_sha256": manifest.SourceSHA256,
				"instrumented_sha256": manifest.InstrumentedSHA256, "tool_versions": manifest.ToolVersions,
				"complete": complete, "calls": engine.calls, "events": engine.events, "runtimes": engine.runtimes,
			})
			require.NoError(t, err)
			file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			require.NoError(t, err, "raw证据只能独占创建")
			_, writeErr := file.Write(payload)
			closeErr := file.Close()
			require.NoError(t, writeErr)
			require.NoError(t, closeErr)
			require.True(t, complete, "覆盖不完整，不能报告通过或覆盖阈值")
		})
		source = string(instrumented)
	}
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, options)
	require.NoError(t, err)
	engine.engine = plugin.Engine
	return &viduTestPlugin{Engine: engine}
}

// Call 保持原返回与普通JS异常，只跟踪显式覆盖模式的创建hook。
func (e *viduTestEngine) Call(ctx context.Context, exportName string, args ...any) (any, error) {
	if !e.coverage || exportName != "buildSubmitRequest" {
		return e.engine.Call(ctx, exportName, args...)
	}
	return e.collectHookCoverage(ctx, exportName, func() (any, error) {
		return e.engine.Call(ctx, exportName, args...)
	})
}

// CallPath 只跟踪旧视频decoder，其它路径与默认模式仍直接调用原Engine。
func (e *viduTestEngine) CallPath(ctx context.Context, exportName string, members []string, args ...any) (any, error) {
	if !e.coverage || exportName != "protocols" || len(members) != 2 || members[0] != "openai_video" || members[1] != "decodeRequest" {
		return e.engine.CallPath(ctx, exportName, members, args...)
	}
	return e.collectHookCoverage(ctx, "protocols.openai_video.decodeRequest", func() (any, error) {
		return e.engine.CallPath(ctx, exportName, members, args...)
	})
}

// collectHookCoverage 在原hook同次finally取得唯一匹配事件，不重新借runtime池。
func (e *viduTestEngine) collectHookCoverage(ctx context.Context, hook string, invoke func() (any, error)) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.activeHook = hook
	defer func() { e.activeHook = "" }()
	before := e.events
	e.calls++
	value, err := invoke()
	var interrupted *sobek.InterruptedError
	if e.events != before+1 || errors.As(err, &interrupted) || ctx.Err() != nil {
		e.incomplete = true
		e.t.Error("覆盖不完整：目标调用未产生恰好一次同步快照或执行已中断")
	}
	return value, err
}
