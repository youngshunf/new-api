package plugins_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	builtinplugins "github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/relay/channel"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoraResponsesProtocol(t *testing.T) {
	testVideoResponsesProtocol(t, videoResponsesTestCase{
		pluginKey: "sora",
		model:     "sora-2-pro",
		requestBody: map[string]any{
			"model":   "sora-2-pro",
			"input":   "waves at sunset",
			"seconds": 8,
			"size":    "1792x1024",
		},
		wantAction: "text_to_video",
		wantRequest: map[string]any{
			"model":   "sora-2-pro",
			"prompt":  "waves at sunset",
			"seconds": float64(8),
			"size":    "1792x1024",
		},
		wantUsageKeys:  []string{"seconds", "size"},
		wantVendorName: "sora",
	})
}

// agnes 走 Sora 方言但不实现 /content：快照带 video_id 无 url 时按 video_id 问自家端点取出片地址；
// 真 Sora（无 video_id）与 New API 网关上游仍走 /v1/videos/{id}/content。
func TestSoraArtifactContentAgnesLookup(t *testing.T) {
	service.InitHttpClient()
	const videoURL = "https://storage.agnes.example/videos/v1.mp4?X-Amz-Signature=sig"
	lookupBodies := map[string]struct {
		status int
		body   string
	}{
		"vid/with space": {http.StatusOK, `{"video_id":"vid/with space","status":"completed","url":"` + videoURL + `"}`},
		"no-url":         {http.StatusOK, `{"video_id":"no-url","status":"completed"}`},
		"denied":         {http.StatusForbidden, `{"error":{"message":"forbidden"}}`},
		"html":           {http.StatusOK, `<html>gateway error</html>`},
	}
	var lookups int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/agnesapi", r.URL.Path)
		assert.Equal(t, "Bearer agnes-key", r.Header.Get("Authorization"))
		answer := lookupBodies[r.URL.Query().Get("video_id")]
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	}))
	defer upstream.Close()

	plugin := loadSoraPlugin(t)
	vendor := taskplugin.New(plugin)
	vendor.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "agnes-key", ChannelBaseUrl: upstream.URL}})
	gateway := taskplugin.New(plugin)
	gateway.Init(&relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: constant.ChannelTypeNewAPI, ApiKey: "sk-gateway", ChannelBaseUrl: "https://gateway.example",
	}})
	proxied := func(base, key string) *channel.TaskContentRequest {
		return &channel.TaskContentRequest{
			URL:     base + "/v1/videos/video_up/content",
			Method:  http.MethodHead,
			Headers: map[string]string{"Authorization": "Bearer " + key},
		}
	}
	direct := &channel.TaskContentRequest{URL: videoURL, Method: http.MethodGet, Credentialless: true}

	for _, tc := range []struct {
		name        string
		adaptor     *taskplugin.TaskAdaptor
		data        map[string]any
		want        *channel.TaskContentRequest
		wantErr     string
		wantLookups int
	}{
		{name: "agnes lookup", adaptor: vendor, data: map[string]any{"id": "video_up", "status": "completed", "video_id": "vid/with space"}, want: direct, wantLookups: 1},
		{name: "agnes snapshot url", adaptor: vendor, data: map[string]any{"id": "video_up", "video_id": "vid", "url": videoURL}, want: direct},
		{name: "agnes lookup without url", adaptor: vendor, data: map[string]any{"id": "video_up", "video_id": "no-url"}, wantErr: "artifact_not_found", wantLookups: 1},
		{name: "agnes lookup denied", adaptor: vendor, data: map[string]any{"id": "video_up", "video_id": "denied"}, wantErr: "agnes video lookup failed: HTTP 403", wantLookups: 1},
		{name: "agnes lookup not json", adaptor: vendor, data: map[string]any{"id": "video_up", "video_id": "html"}, wantErr: "agnes video lookup failed: HTTP 200", wantLookups: 1},
		{name: "real sora", adaptor: vendor, data: map[string]any{"id": "video_up", "status": "completed"}, want: proxied(upstream.URL, "agnes-key")},
		{name: "new api gateway", adaptor: gateway, data: map[string]any{"id": "video_up", "video_id": "vid/with space"}, want: proxied("https://gateway.example", "sk-gateway")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups = 0
			data, err := common.Marshal(tc.data)
			require.NoError(t, err)
			task := &model.Task{TaskID: "task-public", Status: model.TaskStatusSuccess, Data: data,
				PrivateData: model.TaskPrivateData{UpstreamTaskID: "video_up"}}
			descriptor, err := tc.adaptor.BuildContentRequest(t.Context(), task, "video", channel.TaskArtifactClientRequest{Method: http.MethodHead})
			assert.Equal(t, tc.wantLookups, lookups)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, descriptor)
		})
	}
}

func loadSoraPlugin(t *testing.T) *jsplugin.LoadedPlugin {
	t.Helper()
	source, err := builtinplugins.Source("sora")
	require.NoError(t, err)
	plugin, err := jsplugin.NewRegistry().RegisterFactory(source, jsplugin.Options{Key: "sora"})
	require.NoError(t, err)
	return plugin
}

// agnes 视频走 Sora 方言的 /v1/videos，但上游要求 mode 必填、seconds 为字符串、size 只收档位；
// 像素尺寸改写为 aspect_ratio。真 Sora 与 New API 网关上游原样透传。
func TestSoraSubmitAgnesVideoDialect(t *testing.T) {
	plugin := loadSoraPlugin(t)
	submit := func(t *testing.T, ctx map[string]any) (map[string]any, error) {
		t.Helper()
		ctx["baseUrl"] = "https://api.agnes.example"
		ctx["apiKey"] = "agnes-key"
		value, err := plugin.Engine.Call(t.Context(), "buildSubmitRequest", ctx)
		if err != nil {
			return nil, err
		}
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		var descriptor map[string]any
		require.NoError(t, common.Unmarshal(encoded, &descriptor))
		return descriptor, nil
	}
	body := func(t *testing.T, descriptor map[string]any) map[string]any {
		t.Helper()
		value, ok := descriptor["body"].(map[string]any)
		require.True(t, ok, "descriptor body must be a JSON object: %v", descriptor)
		return value
	}

	t.Run("agnes text adds mode and stringifies seconds", func(t *testing.T) {
		descriptor, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5-flash",
			"requestBody":   map[string]any{"model": "agnes-video-2.5-flash", "prompt": "海浪", "seconds": 8},
		})
		require.NoError(t, err)
		assert.Equal(t, "https://api.agnes.example/v1/videos", descriptor["url"])
		assert.Equal(t, map[string]any{"model": "agnes-video-2.5-flash", "prompt": "海浪", "mode": "text", "seconds": "8"}, body(t, descriptor))
	})
	t.Run("agnes pixel size becomes aspect ratio", func(t *testing.T) {
		descriptor, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5",
			"requestBody":   map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "seconds": "4", "size": "720x1280"},
		})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "mode": "text", "seconds": "4", "aspect_ratio": "9:16"}, body(t, descriptor))
	})
	t.Run("agnes tier size and explicit mode pass through, omitted seconds sends billed default", func(t *testing.T) {
		descriptor, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5",
			"requestBody":   map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "mode": "text", "size": "1080P"},
		})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "mode": "text", "seconds": "4", "size": "1080P"}, body(t, descriptor))
	})
	t.Run("agnes unknown pixel size is rejected before dispatch", func(t *testing.T) {
		_, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5",
			"requestBody":   map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "size": "1234x567"},
		})
		require.ErrorContains(t, err, "agnes video does not support size 1234x567")
	})
	t.Run("agnes first frame file is rejected before dispatch", func(t *testing.T) {
		_, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5",
			"requestBody":   map[string]any{"model": "agnes-video-2.5", "prompt": "海浪"},
			"files":         []any{map[string]any{"field": "input_reference", "ref": "f0", "filename": "a.png"}},
		})
		require.ErrorContains(t, err, "agnes video only supports text mode")
	})
	t.Run("real sora passes through unchanged", func(t *testing.T) {
		descriptor, err := submit(t, map[string]any{
			"upstreamModel": "sora-2",
			"requestBody":   map[string]any{"model": "sora-2", "prompt": "海浪", "seconds": 8, "size": "1280x720"},
		})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"model": "sora-2", "prompt": "海浪", "seconds": float64(8), "size": "1280x720"}, body(t, descriptor))
	})
	t.Run("new api gateway upstream passes through unchanged", func(t *testing.T) {
		descriptor, err := submit(t, map[string]any{
			"upstreamModel": "agnes-video-2.5",
			"upstream":      map[string]any{"kind": "new_api"},
			"requestBody":   map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "seconds": 8},
		})
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"model": "agnes-video-2.5", "prompt": "海浪", "seconds": float64(8)}, body(t, descriptor))
	})
}
