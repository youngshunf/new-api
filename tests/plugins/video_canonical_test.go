package plugins_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestViduCanonicalVideoHook(t *testing.T) {
	plugin := newViduPluginForTest(t)

	t.Run("missing_generation_mode_rejected", func(t *testing.T) {
		_, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl":       "http://127.0.0.1:1",
			"upstreamModel": "viduq2",
			"requestBody": map[string]any{
				"model":  "viduq2",
				"prompt": "保持参考主体运动",
				"reference_images": []any{
					map[string]any{"__fileRef": "request_file:reference_image_0", "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": 15728640},
				},
			},
		})
		require.Error(t, callErr, "角色在场而缺模式必须拒绝，不能退回旧文生分支")
		assert.Contains(t, callErr.Error(), "generation_mode")
	})

	t.Run("two_references_remain_reference_to_video", func(t *testing.T) {
		references := []any{
			map[string]any{"__fileRef": "request_file:reference_image_0", "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": 15728640},
			map[string]any{"__fileRef": "request_file:reference_image_1", "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": 15728640},
		}
		value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl":       "http://127.0.0.1:1",
			"upstreamModel": "viduq2",
			"requestBody": map[string]any{
				"model":            "viduq2",
				"prompt":           "保持两张参考图的主体与风格",
				"generation_mode":  "reference_to_video",
				"reference_images": references,
			},
		})
		require.NoError(t, callErr)
		var descriptor struct {
			Action string         `json:"action"`
			URL    string         `json:"url"`
			Body   map[string]any `json:"body"`
		}
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, common.Unmarshal(encoded, &descriptor))
		assert.Equal(t, "reference_to_video", descriptor.Action)
		assert.Equal(t, "http://127.0.0.1:1/ent/v2/reference2video", descriptor.URL)
		require.NotNil(t, descriptor.Body)
		body := descriptor.Body
		expectedJSON, err := common.Marshal(references)
		require.NoError(t, err)
		actualJSON, err := common.Marshal(body["images"])
		require.NoError(t, err)
		assert.JSONEq(t, string(expectedJSON), string(actualJSON), "两项参考必须保序，不得改成首尾帧")
		assert.NotContains(t, body, "generation_mode")
		assert.NotContains(t, body, "reference_images")
	})

	t.Run("canonical_jpeg_placeholder_accepted", func(t *testing.T) {
		firstFrame := viduCanonicalFile("first_frame")
		firstFrame["mimeType"] = "image/jpeg"
		value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl": "http://127.0.0.1:1", "upstreamModel": "viduq2",
			"requestBody": map[string]any{
				"generation_mode": "first_frame_to_video", "model": "viduq2", "prompt": "保留宿主规范JPEG占位",
				"duration": 5, "size": "720p", "first_frame": firstFrame,
			},
		})
		require.NoError(t, callErr)
		encoded, err := common.Marshal(value)
		require.NoError(t, err)
		var descriptor struct {
			Action string         `json:"action"`
			URL    string         `json:"url"`
			Body   map[string]any `json:"body"`
		}
		require.NoError(t, common.Unmarshal(encoded, &descriptor))
		assert.Equal(t, "image_to_video", descriptor.Action)
		assert.Equal(t, "http://127.0.0.1:1/ent/v2/img2video", descriptor.URL)
		expected, err := common.Marshal([]any{firstFrame})
		require.NoError(t, err)
		actual, err := common.Marshal(descriptor.Body["images"])
		require.NoError(t, err)
		assert.JSONEq(t, string(expected), string(actual), "JPEG占位不得误拒或被改写成PNG；此处不证明真实文件解码")
	})
}

func TestViduCanonicalVideoModes(t *testing.T) {
	plugin := newViduPluginForTest(t)

	for _, testCase := range []struct {
		mode       string
		roles      map[string]any
		wantAction string
		wantPath   string
		wantImages []any
	}{
		{mode: "text_to_video", wantAction: "text_to_video", wantPath: "/text2video"},
		{
			mode:       "reference_to_video",
			roles:      map[string]any{"reference_images": []any{viduCanonicalFile("reference_image_0"), viduCanonicalFile("reference_image_1")}},
			wantAction: "reference_to_video", wantPath: "/reference2video",
			wantImages: []any{viduCanonicalFile("reference_image_0"), viduCanonicalFile("reference_image_1")},
		},
		{
			mode:       "first_frame_to_video",
			roles:      map[string]any{"first_frame": viduCanonicalFile("first_frame")},
			wantAction: "image_to_video", wantPath: "/img2video",
			wantImages: []any{viduCanonicalFile("first_frame")},
		},
		{
			mode:       "first_last_frame_to_video",
			roles:      map[string]any{"first_frame": viduCanonicalFile("first_frame"), "last_frame": viduCanonicalFile("last_frame")},
			wantAction: "first_tail_to_video", wantPath: "/start-end2video",
			wantImages: []any{viduCanonicalFile("first_frame"), viduCanonicalFile("last_frame")},
		},
	} {
		t.Run(testCase.mode, func(t *testing.T) {
			request := map[string]any{"generation_mode": testCase.mode, "model": "viduq2", "prompt": "保持原角色", "duration": 5, "size": "720p"}
			for name, value := range testCase.roles {
				request[name] = value
			}
			before, err := common.Marshal(request)
			require.NoError(t, err)
			value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"baseUrl": "http://127.0.0.1:1", "upstreamModel": "viduq2", "requestBody": request,
			})
			require.NoError(t, callErr)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var descriptor struct {
				Action string         `json:"action"`
				URL    string         `json:"url"`
				Body   map[string]any `json:"body"`
			}
			require.NoError(t, common.Unmarshal(encoded, &descriptor))
			assert.Equal(t, testCase.wantAction, descriptor.Action)
			assert.Equal(t, "http://127.0.0.1:1/ent/v2"+testCase.wantPath, descriptor.URL)
			expected, err := common.Marshal(testCase.wantImages)
			require.NoError(t, err)
			actual, err := common.Marshal(descriptor.Body["images"])
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual))
			assert.Equal(t, "viduq2", descriptor.Body["model"])
			assert.Equal(t, "保持原角色", descriptor.Body["prompt"])
			assert.Equal(t, float64(5), descriptor.Body["duration"])
			assert.Equal(t, "720p", descriptor.Body["resolution"])
			for _, field := range []string{"generation_mode", "reference_images", "first_frame", "last_frame", "metadata", "action"} {
				assert.NotContains(t, descriptor.Body, field)
			}
			after, err := common.Marshal(request)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after), "插件构造不得修改冻结请求")
		})
	}
}

func TestViduCanonicalVideoRejectsInvalidRoles(t *testing.T) {
	plugin := newViduPluginForTest(t)

	tooManyReferences := make([]any, 17)
	for index := range tooManyReferences {
		tooManyReferences[index] = viduCanonicalFile("reference_image_0")
	}
	for _, testCase := range []struct {
		name    string
		fields  map[string]any
		message string
	}{
		{name: "mode_null", fields: map[string]any{"generation_mode": nil}, message: "generation_mode"},
		{name: "mode_empty", fields: map[string]any{"generation_mode": ""}, message: "generation_mode"},
		{name: "mode_whitespace", fields: map[string]any{"generation_mode": " text_to_video "}, message: "generation_mode"},
		{name: "mode_wrong_type", fields: map[string]any{"generation_mode": 1}, message: "generation_mode"},
		{name: "mode_unknown", fields: map[string]any{"generation_mode": "referenceGenerate"}, message: "generation_mode"},
		{name: "image_mode", fields: map[string]any{"generation_mode": "text_to_image"}, message: "generation_mode"},
		{name: "image_edit_mode", fields: map[string]any{"generation_mode": "image_to_image"}, message: "generation_mode"},
		{name: "role_only_null", fields: map[string]any{"first_frame": nil}, message: "generation_mode"},
		{name: "text_reference_null", fields: map[string]any{"generation_mode": "text_to_video", "reference_images": nil}, message: "forbids image roles"},
		{name: "text_reference_empty", fields: map[string]any{"generation_mode": "text_to_video", "reference_images": []any{}}, message: "forbids image roles"},
		{name: "text_first_frame", fields: map[string]any{"generation_mode": "text_to_video", "first_frame": viduCanonicalFile("first_frame")}, message: "forbids image roles"},
		{name: "text_last_null", fields: map[string]any{"generation_mode": "text_to_video", "last_frame": nil}, message: "forbids image roles"},
		{name: "references_missing", fields: map[string]any{"generation_mode": "reference_to_video"}, message: "reference_images"},
		{name: "references_empty", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{}}, message: "reference_images"},
		{name: "references_null", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": nil}, message: "reference_images"},
		{name: "references_wrong_type", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": viduCanonicalFile("reference_image_0")}, message: "reference_images"},
		{name: "references_over_limit", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": tooManyReferences}, message: "reference_images"},
		{name: "reference_null_item", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{nil}}, message: "file placeholder"},
		{name: "reference_string", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{"https://invalid.example/image.png"}}, message: "file placeholder"},
		{name: "references_duplicate", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{viduCanonicalFile("reference_image_0"), viduCanonicalFile("reference_image_0")}}, message: "duplicate"},
		{name: "references_wrong_order", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{viduCanonicalFile("reference_image_1"), viduCanonicalFile("reference_image_0")}}, message: "match role"},
		{name: "metadata_role_override", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{viduCanonicalFile("reference_image_0")}, "metadata": map[string]any{"model": "other", "action": "firstTailGenerate", "images": []any{"override"}}}, message: "canonical request cannot contain metadata"},
		{name: "reference_and_first", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{viduCanonicalFile("reference_image_0")}, "first_frame": nil}, message: "forbids frame roles"},
		{name: "reference_and_last", fields: map[string]any{"generation_mode": "reference_to_video", "reference_images": []any{viduCanonicalFile("reference_image_0")}, "last_frame": nil}, message: "forbids frame roles"},
		{name: "first_missing", fields: map[string]any{"generation_mode": "first_frame_to_video"}, message: "first_frame is required"},
		{name: "first_null", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": nil}, message: "file placeholder"},
		{name: "first_array", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": []any{viduCanonicalFile("first_frame")}}, message: "file placeholder"},
		{name: "first_and_reference_empty", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": viduCanonicalFile("first_frame"), "reference_images": []any{}}, message: "forbids reference_images"},
		{name: "first_and_last_null", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": viduCanonicalFile("first_frame"), "last_frame": nil}, message: "forbids reference_images and last_frame"},
		{name: "first_last_without_last", fields: map[string]any{"generation_mode": "first_last_frame_to_video", "first_frame": viduCanonicalFile("first_frame")}, message: "first_frame and last_frame are required"},
		{name: "last_only", fields: map[string]any{"generation_mode": "first_last_frame_to_video", "last_frame": viduCanonicalFile("last_frame")}, message: "first_frame and last_frame are required"},
		{name: "first_last_null_last", fields: map[string]any{"generation_mode": "first_last_frame_to_video", "first_frame": viduCanonicalFile("first_frame"), "last_frame": nil}, message: "file placeholder"},
		{name: "first_last_with_reference", fields: map[string]any{"generation_mode": "first_last_frame_to_video", "first_frame": viduCanonicalFile("first_frame"), "last_frame": viduCanonicalFile("last_frame"), "reference_images": nil}, message: "forbids reference_images"},
		{name: "first_wrong_ref", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": viduCanonicalFile("last_frame")}, message: "match role"},
		{name: "arbitrary_object", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": map[string]any{"url": "ignored"}}, message: "placeholder field"},
		{name: "wrong_encoding", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "base64", "mimeType": "image/png", "maxBytes": 15728640}}, message: "invalid image file placeholder"},
		{name: "unbounded_file", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": 15728641}}, message: "maxBytes"},
		{name: "missing_file_limit", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "mimeType": "image/png"}}, message: "maxBytes"},
		{name: "empty_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": ""}}, message: "mimeType"},
		// 规范形只接受宿主提供的精确image/png或image/jpeg；每项仅破坏MIME，不代替文件内容核验。
		{name: "missing_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640}}, message: "mimeType"},
		{name: "null_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": nil}}, message: "mimeType"},
		{name: "wrong_type_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": 1}}, message: "mimeType"},
		{name: "text_plain_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": "text/plain"}}, message: "mimeType"},
		{name: "image_gif_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": "image/gif"}}, message: "mimeType"},
		{name: "parameterized_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": "image/png; charset=utf-8"}}, message: "mimeType"},
		{name: "noncanonical_case_mime", fields: map[string]any{"generation_mode": "first_frame_to_video", "duration": 5, "size": "720p", "first_frame": map[string]any{"__fileRef": "request_file:first_frame", "encoding": "dataUrl", "maxBytes": 15728640, "mimeType": "Image/PNG"}}, message: "mimeType"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := map[string]any{"model": "viduq2", "prompt": "拒绝冲突角色"}
			for name, value := range testCase.fields {
				request[name] = value
			}
			_, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"baseUrl": "http://127.0.0.1:1", "upstreamModel": "viduq2", "requestBody": request,
			})
			require.Error(t, callErr)
			assert.Contains(t, callErr.Error(), testCase.message)
		})
	}

	t.Run("existing_vidu_combo_rejection_kept", func(t *testing.T) {
		_, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl": "http://127.0.0.1:1", "upstreamModel": "vidu2.0",
			"requestBody": map[string]any{"generation_mode": "text_to_video", "model": "vidu2.0", "prompt": "不放宽原供应商限制"},
		})
		require.Error(t, callErr)
		assert.Contains(t, callErr.Error(), "vidu2.0 does not support text-to-video")
	})

	for _, field := range []string{"mode", "image", "images", "input_reference", "metadata", "action"} {
		t.Run("mixed_legacy_"+field, func(t *testing.T) {
			_, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"baseUrl": "http://127.0.0.1:1", "upstreamModel": "viduq2",
				"requestBody": map[string]any{"generation_mode": "text_to_video", "model": "viduq2", "prompt": "拒绝旧字段", field: nil},
			})
			require.Error(t, callErr, "旧字段即使为null也不得与canonical混用")
			assert.Contains(t, callErr.Error(), "canonical request cannot contain "+field)
		})
	}
}

func TestViduLegacyVideoHookUnchanged(t *testing.T) {
	plugin := newViduPluginForTest(t)

	for _, testCase := range []struct {
		name       string
		images     []any
		metadata   map[string]any
		wantAction string
		wantPath   string
	}{
		{name: "text_by_count", wantAction: "text_to_video", wantPath: "/text2video"},
		{name: "single_image_by_count", images: []any{"legacy-first"}, wantAction: "image_to_video", wantPath: "/img2video"},
		{name: "two_images_by_count", images: []any{"legacy-first", "legacy-last"}, wantAction: "first_tail_to_video", wantPath: "/start-end2video"},
		{name: "three_images_by_count", images: []any{"legacy-one", "legacy-two", "legacy-three"}, wantAction: "reference_to_video", wantPath: "/reference2video"},
		{name: "reference_alias_with_two_images", images: []any{"legacy-one", "legacy-two"}, metadata: map[string]any{"action": "referenceGenerate"}, wantAction: "reference_to_video", wantPath: "/reference2video"},
		{name: "first_tail_alias", images: []any{"legacy-first", "legacy-last"}, metadata: map[string]any{"action": "firstTailGenerate"}, wantAction: "first_tail_to_video", wantPath: "/start-end2video"},
		{name: "text_alias", metadata: map[string]any{"action": "textGenerate"}, wantAction: "text_to_video", wantPath: "/text2video"},
		{name: "image_alias", images: []any{"legacy-first"}, metadata: map[string]any{"action": "generate"}, wantAction: "image_to_video", wantPath: "/img2video"},
		{name: "metadata_body_override", images: []any{"legacy-original"}, metadata: map[string]any{"action": "referenceGenerate", "model": "legacy-vendor-model", "images": []any{"legacy-overridden"}}, wantAction: "reference_to_video", wantPath: "/reference2video"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := map[string]any{"model": "viduq2", "prompt": "保留旧入口语义"}
			if testCase.images != nil {
				request["images"] = testCase.images
			}
			if testCase.metadata != nil {
				request["metadata"] = testCase.metadata
			}
			value, callErr := plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
				"baseUrl": "http://127.0.0.1:1", "upstreamModel": "viduq2", "requestBody": request,
			})
			require.NoError(t, callErr)
			encoded, err := common.Marshal(value)
			require.NoError(t, err)
			var descriptor struct {
				Action string         `json:"action"`
				URL    string         `json:"url"`
				Body   map[string]any `json:"body"`
			}
			require.NoError(t, common.Unmarshal(encoded, &descriptor))
			assert.Equal(t, testCase.wantAction, descriptor.Action)
			assert.Equal(t, "http://127.0.0.1:1/ent/v2"+testCase.wantPath, descriptor.URL)
			var expectedImages any = testCase.images
			expectedModel := "viduq2"
			if override, exists := testCase.metadata["images"]; exists {
				expectedImages = override
			}
			// 上游已收紧：metadata 只能带 Vidu 其它字段，不能改写计费模型、时长与分辨率。
			expected, err := common.Marshal(expectedImages)
			require.NoError(t, err)
			actual, err := common.Marshal(descriptor.Body["images"])
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual))
			assert.Equal(t, expectedModel, descriptor.Body["model"])
			assert.NotContains(t, descriptor.Body, "action")
		})
	}
}

func TestViduLegacyEndpointRejectsCanonicalFields(t *testing.T) {
	plugin := newViduPluginForTest(t)

	for _, role := range []struct {
		field string
		value any
	}{
		{field: "generation_mode", value: "reference_to_video"},
		{field: "reference_images", value: []any{"https://legacy.example/reference.png"}},
		{field: "first_frame", value: "https://legacy.example/first.png"},
		{field: "last_frame", value: "https://legacy.example/last.png"},
	} {
		for _, presence := range []struct {
			name  string
			value any
		}{
			{name: "present", value: role.value},
			{name: "null", value: nil},
		} {
			t.Run(role.field+"_"+presence.name, func(t *testing.T) {
				// 旧入口只按键存在性拒绝规范角色；null也不能变成悄悄接受或忽略。
				_, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
					"model": "viduq2", "upstreamModel": "viduq2",
					"body": map[string]any{"kind": "json", "value": map[string]any{
						"model": "viduq2", "prompt": "旧端点不得接收规范角色", "seconds": 5, "size": "720p",
						"images": []any{"https://legacy.example/input.png"}, role.field: presence.value,
					}},
				})
				require.Error(t, callErr, "旧openai_video入口不得接收任何canonical键")
				assert.Contains(t, callErr.Error(), "openai_video does not accept canonical field: "+role.field)
			})
		}
	}

	t.Run("legacy_json_decode_then_submit_preserved", func(t *testing.T) {
		images := []any{"https://legacy.example/first.png", "https://legacy.example/last.png"}
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "viduq2", "upstreamModel": "viduq2",
			"body": map[string]any{"kind": "json", "value": map[string]any{
				"model": "viduq2", "prompt": "保留旧首尾帧请求", "seconds": 8, "size": "720p", "images": images,
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
		assert.Equal(t, "submit", decoded.Kind)
		assert.Equal(t, "viduq2", decoded.Model)
		assert.Equal(t, "first_tail_to_video", decoded.Action)
		expectedDecoded, err := common.Marshal(map[string]any{
			"model": "viduq2", "prompt": "保留旧首尾帧请求", "seconds": 8, "duration": 8,
			"size": "720p", "resolution": "720p", "images": images,
		})
		require.NoError(t, err)
		actualDecoded, err := common.Marshal(decoded.RequestBody)
		require.NoError(t, err)
		assert.JSONEq(t, string(expectedDecoded), string(actualDecoded), "旧decoder必须完整保留原字段及seconds转换")

		// 必须使用真实decode结果组合调用，不用手写的期望body替代接缝。
		value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl": "http://127.0.0.1:1", "model": decoded.Model, "upstreamModel": "viduq2",
			"action": decoded.Action, "requestBody": decoded.RequestBody,
		})
		require.NoError(t, callErr)
		encoded, err = common.Marshal(value)
		require.NoError(t, err)
		var descriptor struct {
			Action string         `json:"action"`
			URL    string         `json:"url"`
			Method string         `json:"method"`
			Body   map[string]any `json:"body"`
		}
		require.NoError(t, common.Unmarshal(encoded, &descriptor))
		assert.Equal(t, "first_tail_to_video", descriptor.Action)
		assert.Equal(t, "POST", descriptor.Method)
		assert.Equal(t, "http://127.0.0.1:1/ent/v2/start-end2video", descriptor.URL)
		expectedBody, err := common.Marshal(map[string]any{
			"model": "viduq2", "prompt": "保留旧首尾帧请求", "duration": 8, "resolution": "720p",
			"images": images, "movement_amplitude": "auto",
		})
		require.NoError(t, err)
		actualBody, err := common.Marshal(descriptor.Body)
		require.NoError(t, err)
		assert.JSONEq(t, string(expectedBody), string(actualBody), "组合提交body必须保持旧语义且无canonical角色字段")
	})

	for _, role := range []struct {
		field string
		value string
	}{
		{field: "generation_mode", value: "first_frame_to_video"},
		{field: "reference_images", value: `["reference_image_0"]`},
		{field: "first_frame", value: "first_frame"},
		{field: "last_frame", value: "last_frame"},
	} {
		t.Run("multipart_"+role.field+"_rejected", func(t *testing.T) {
			_, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
				"model": "viduq2", "upstreamModel": "viduq2",
				"body": map[string]any{
					"kind": "multipart",
					"fields": map[string]any{
						"model": []string{"viduq2"}, "prompt": []string{"旧multipart不得接收规范角色"},
						"seconds": []string{"5"}, "size": []string{"720p"}, role.field: []string{role.value},
					},
					"files": []any{map[string]any{"field": "input_reference", "ref": "request_file:input_reference"}},
				},
			})
			require.Error(t, callErr, "旧multipart文本角色键必须在decoder拒绝")
			assert.Contains(t, callErr.Error(), "openai_video does not accept canonical field: "+role.field)
		})
	}

	t.Run("legacy_multipart_decode_then_submit_preserved", func(t *testing.T) {
		// 仅消费宿主传入的文件描述及decoder生成的旧内部占位，不读文件、不物化或发送请求。
		value, callErr := plugin.Engine.CallPath(t.Context(), "protocols", []string{"openai_video", "decodeRequest"}, map[string]any{
			"model": "viduq2", "upstreamModel": "viduq2",
			"body": map[string]any{
				"kind": "multipart",
				"fields": map[string]any{
					"model": []string{"viduq2"}, "prompt": []string{"保留旧文件首帧请求"},
					"seconds": []string{"5"}, "size": []string{"720p"},
				},
				"files": []any{map[string]any{"field": "input_reference", "ref": "request_file:input_reference"}},
			},
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
		assert.Equal(t, "submit", decoded.Kind)
		assert.Equal(t, "viduq2", decoded.Model)
		assert.Equal(t, "image_to_video", decoded.Action)
		images := []any{map[string]any{
			"__fileRef": "request_file:input_reference", "encoding": "dataUrl", "maxBytes": 15728640,
		}}
		expectedDecoded, err := common.Marshal(map[string]any{
			"model": "viduq2", "prompt": "保留旧文件首帧请求", "seconds": 5, "duration": 5,
			"size": "720p", "resolution": "720p", "images": images,
		})
		require.NoError(t, err)
		actualDecoded, err := common.Marshal(decoded.RequestBody)
		require.NoError(t, err)
		assert.JSONEq(t, string(expectedDecoded), string(actualDecoded), "旧multipart必须保留唯一input_reference占位及seconds转换")

		value, callErr = plugin.Engine.Call(t.Context(), "buildSubmitRequest", map[string]any{
			"baseUrl": "http://127.0.0.1:1", "model": decoded.Model, "upstreamModel": "viduq2",
			"action": decoded.Action, "requestBody": decoded.RequestBody,
		})
		require.NoError(t, callErr)
		encoded, err = common.Marshal(value)
		require.NoError(t, err)
		var descriptor struct {
			Action string         `json:"action"`
			URL    string         `json:"url"`
			Method string         `json:"method"`
			Body   map[string]any `json:"body"`
		}
		require.NoError(t, common.Unmarshal(encoded, &descriptor))
		assert.Equal(t, "image_to_video", descriptor.Action)
		assert.Equal(t, "POST", descriptor.Method)
		assert.Equal(t, "http://127.0.0.1:1/ent/v2/img2video", descriptor.URL)
		expectedBody, err := common.Marshal(map[string]any{
			"model": "viduq2", "prompt": "保留旧文件首帧请求", "duration": 5, "resolution": "720p",
			"images": images, "movement_amplitude": "auto",
		})
		require.NoError(t, err)
		actualBody, err := common.Marshal(descriptor.Body)
		require.NoError(t, err)
		assert.JSONEq(t, string(expectedBody), string(actualBody), "旧文件占位必须原样进入单首帧供应商body，不套用canonical的必需MIME门")
	})
}

// viduCanonicalFile 仅构造宿主准入之后的内部占位输入，不代表已核真实文件或供应商能力。
func viduCanonicalFile(field string) map[string]any {
	return map[string]any{"__fileRef": "request_file:" + field, "encoding": "dataUrl", "mimeType": "image/png", "maxBytes": 15728640}
}
