package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestViduResponsesDecodeThenSubmitCompatibility(t *testing.T) {
	plugin := newViduPluginForTest(t)
	value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "decodeRequest"}, map[string]any{
		"model": "viduq2", "stream": false,
		"body": map[string]any{"kind": "json", "value": map[string]any{
			"model": "viduq2",
			"input": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "move between frames"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/first.png"},
				map[string]any{"type": "input_image", "image_url": "https://cdn.example/last.png"},
			}}},
			"seconds": 8, "size": "720p",
		}},
	})
	require.NoError(t, callErr)
	encoded, err := common.Marshal(value)
	require.NoError(t, err)
	var decoded struct {
		Kind        string         `json:"kind"`
		Model       string         `json:"model"`
		Action      string         `json:"action"`
		RequestBody map[string]any `json:"requestBody"`
	}
	require.NoError(t, common.Unmarshal(encoded, &decoded))
	assert.JSONEq(t, `{
		"kind":"submit","model":"viduq2","action":"first_tail_to_video",
		"requestBody":{"model":"viduq2","prompt":"move between frames",
			"images":["https://cdn.example/first.png","https://cdn.example/last.png"],"duration":8,"size":"720p"}
	}`, string(encoded), "Responses decoder必须产出完整旧形，不引入canonical角色")

	// 组合直接消费真实decode结果；只构造供应商请求描述，不发送HTTP或读取图片URL。
	value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
		"baseUrl": "http://127.0.0.1:1", "apiKey": "unit-test-key", "model": decoded.Model,
		"upstreamModel": "viduq2", "action": decoded.Action, "requestBody": decoded.RequestBody,
	})
	require.NoError(t, callErr)
	encoded, err = common.Marshal(value)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"url":"http://127.0.0.1:1/ent/v2/start-end2video","method":"POST","action":"first_tail_to_video",
		"headers":{"Content-Type":"application/json","Accept":"application/json","Authorization":"Token unit-test-key"},
		"body":{"model":"viduq2","images":["https://cdn.example/first.png","https://cdn.example/last.png"],
			"prompt":"move between frames","duration":8,"resolution":"720p","movement_amplitude":"auto"}
	}`, string(encoded), "Responses旧形必须经真实build保留首尾动作、图序、数值和完整供应商描述")
}

func TestViduResponsesRenderCompatibility(t *testing.T) {
	plugin := newViduPluginForTest(t)
	protocolContext := map[string]any{
		"requestBody": map[string]any{"model": "viduq2"}, "stream": true,
		"artifacts": map[string]any{"video": map[string]any{
			"key": "video", "type": "video", "mimeType": "video/mp4",
			"url": "https://gateway.example/v1/tasks/task_public/artifacts/video/content?access=host%2Bcapability%3D",
		}},
	}
	successTask := map[string]any{
		"task_id": "task_public", "status": "SUCCESS", "progress": "100%", "created_at": 10, "updated_at": 20,
		"data": map[string]any{"url": "https://upstream.example/video.mp4?signature=must-not-leak"},
	}
	for _, testCase := range []struct {
		name     string
		task     map[string]any
		previous map[string]any
		expected string
	}{
		{
			name: "progress_event",
			task: map[string]any{"task_id": "task_public", "status": "IN_PROGRESS", "progress": "42%"},
			expected: `{"events":[{"type":"progress","message":"in_progress","progress":42}],
				"state":{"status":"IN_PROGRESS","progress":42},"done":false}`,
		},
		{
			name:     "duplicate_progress_suppressed",
			task:     map[string]any{"task_id": "task_public", "status": "IN_PROGRESS", "progress": "42%"},
			previous: map[string]any{"status": "IN_PROGRESS", "progress": 42},
			expected: `{"events":[],"state":{"status":"IN_PROGRESS","progress":42},"done":false}`,
		},
		{
			name: "failure_is_terminal",
			task: map[string]any{"task_id": "task_public", "status": "FAILURE", "fail_reason": "blocked"},
			expected: `{"events":[{"type":"error","code":"task_failed","message":"blocked"}],
				"state":{"status":"FAILURE","progress":0},"done":true}`,
		},
		{
			name: "success_uses_host_artifact", task: successTask,
			expected: `{"events":[{"type":"output","data":"<video controls src=\"https://gateway.example/v1/tasks/task_public/artifacts/video/content?access=host%2Bcapability%3D\"></video>"}],
				"state":{"status":"SUCCESS","progress":100},"done":true}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderEvents"},
				protocolContext, testCase.task, testCase.previous)
			require.NoError(t, callErr)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			assert.JSONEq(t, testCase.expected, string(encoded), "只核真实JS事件结构，不复制宿主Go状态机")
			assert.NotContains(t, string(encoded), "upstream.example")
		})
	}

	t.Run("final_uses_host_artifact", func(t *testing.T) {
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", "renderFinal"},
			protocolContext, successTask)
		require.NoError(t, callErr)
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"output":[{"type":"message","status":"completed","role":"assistant",
				"content":[{"type":"output_text","text":"<video controls src=\"https://gateway.example/v1/tasks/task_public/artifacts/video/content?access=host%2Bcapability%3D\"></video>","annotations":[],"logprobs":[]}]}],
			"metadata":{"vendor":"vidu"}
		}`, string(encoded), "只核原renderer输出，不宣称Go最终响应envelope已验证")
		assert.NotContains(t, string(encoded), "upstream.example")
	})

	for _, hook := range []string{"renderEvents", "renderFinal"} {
		t.Run(hook+"_requires_host_artifact", func(t *testing.T) {
			// task仍含上游成功URL，缺宿主artifact必须报错，不能隐式回退上游直链。
			_, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_responses", hook},
				map[string]any{"requestBody": map[string]any{"model": "viduq2"}}, successTask)
			require.ErrorContains(t, callErr, "video artifact is unavailable")
		})
	}
}
