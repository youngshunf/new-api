package sora

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoraBuildRequestBodyReturnsReplayablePassThroughBody(t *testing.T) {
	payload := []byte("opaque-sora-request-body")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/octet-stream")
	defer common.CleanupBodyStorage(c)

	info := &relaycommon.RelayInfo{}
	body, err := (&TaskAdaptor{}).BuildRequestBody(c, info)
	require.NoError(t, err)
	replayable, ok := body.(common.ReplayableBody)
	require.True(t, ok)

	sent, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, payload, sent)
	assert.EqualValues(t, len(payload), replayable.Size())

	replayBody, err := replayable.NewReader()
	require.NoError(t, err)
	replay, err := io.ReadAll(replayBody)
	require.NoError(t, err)
	require.NoError(t, replayBody.Close())
	assert.Equal(t, payload, replay)
}

// 上游若在查询响应里直接给出出片 URL（不实现 OpenAI 的 /v1/videos/{id}/content 下载端点的那类），
// ParseTaskResult 必须把它带出去——否则上层会回落到代理下载地址，而那个端点在这类上游上取不到文件。
func TestSoraParseTaskResultCarriesUpstreamVideoURL(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantStat int
		wantURL  string
	}{
		{
			name:     "响应带 url 时透出该 url",
			body:     `{"id":"task_1","status":"completed","progress":100,"url":"https://cdn.example.com/videos/v1.mp4"}`,
			wantStat: 3, // model.TaskStatusSuccess
			wantURL:  "https://cdn.example.com/videos/v1.mp4",
		},
		{
			// 真 OpenAI Sora 的形状：没有 url 字段，必须留空以回落到 /content 代理下载
			name:     "响应无 url 时留空以回落代理下载",
			body:     `{"id":"video_1","status":"completed","progress":100}`,
			wantStat: 3,
			wantURL:  "",
		},
		{
			name:     "未完成时不带 url",
			body:     `{"id":"task_2","status":"in_progress","progress":30}`,
			wantStat: 2, // model.TaskStatusInProgress
			wantURL:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := (&TaskAdaptor{}).ParseTaskResult([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, info.Url)
		})
	}
}

// legacy 查询端点只回 video_id、不回 url 时，必须改问上游自家端点取出片 URL；
// 真 OpenAI Sora（无 video_id 字段）不得触发该兜底请求。
func TestSoraFetchTaskFallsBackToUpstreamOwnEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		legacyBody   string
		altStatus    int
		altBody      string
		wantAltHit   bool
		wantFinalURL string
	}{
		{
			name:         "只回 video_id 时改问自家端点并采用其响应",
			legacyBody:   `{"id":"task_1","status":"completed","progress":100,"video_id":"video_abc"}`,
			altStatus:    http.StatusOK,
			altBody:      `{"id":"video_abc","status":"completed","progress":100,"url":"https://cdn.example.com/a.mp4"}`,
			wantAltHit:   true,
			wantFinalURL: "https://cdn.example.com/a.mp4",
		},
		{
			name:         "legacy 已带 url 时不再多打一次",
			legacyBody:   `{"id":"task_2","status":"completed","progress":100,"video_id":"video_x","url":"https://cdn.example.com/b.mp4"}`,
			wantAltHit:   false,
			wantFinalURL: "https://cdn.example.com/b.mp4",
		},
		{
			name:         "真 Sora 形状（无 video_id）不触发兜底",
			legacyBody:   `{"id":"video_native","status":"completed","progress":100}`,
			wantAltHit:   false,
			wantFinalURL: "",
		},
		{
			name:       "自家端点失败时回落 legacy 回包，不中断轮询",
			legacyBody: `{"id":"task_3","status":"completed","progress":100,"video_id":"video_err"}`,
			altStatus:  http.StatusInternalServerError,
			altBody:    `{"error":"boom"}`,
			wantAltHit: true,
			// 兜底失败 → 仍用 legacy 回包，其中没有 url
			wantFinalURL: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			altHit := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer k-1", r.Header.Get("Authorization"))
				if r.URL.Path == "/agnesapi" {
					altHit = true
					assert.NotEmpty(t, r.URL.Query().Get("video_id"), "必须按 video_id 查询")
					w.WriteHeader(tc.altStatus)
					_, _ = w.Write([]byte(tc.altBody))
					return
				}
				assert.Equal(t, "/v1/videos/task-under-test", r.URL.Path)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.legacyBody))
			}))
			defer srv.Close()

			resp, err := (&TaskAdaptor{}).FetchTask(srv.URL, "k-1", map[string]any{"task_id": "task-under-test"}, "")
			require.NoError(t, err)
			require.NotNil(t, resp)
			defer resp.Body.Close()

			payload, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAltHit, altHit, "是否请求了上游自家端点")

			info, err := (&TaskAdaptor{}).ParseTaskResult(payload)
			require.NoError(t, err)
			assert.Equal(t, tc.wantFinalURL, info.Url)
		})
	}
}
