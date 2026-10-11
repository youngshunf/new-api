package servicetests

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVoiceCreationRequestRejectsAmbiguityAndBounds(t *testing.T) {
	valid := `{"kind":"design","model":"cosyvoice-v3.5-plus","language":"zh","description":"沉稳的声音","preview_text":"这是一段符合长度约束的完整试听文本。"}`
	for _, tc := range []struct {
		name, body, key string
		valid           bool
	}{
		{"valid", valid, "stable-key", true},
		{"no-key", valid, "", false},
		{"duplicate", strings.Replace(valid, `"kind":"design"`, `"kind":"clone","kind":"design"`, 1), "key", false},
		{"case-alias", strings.Replace(valid, `"kind"`, `"Kind"`, 1), "key", false},
		{"null", strings.Replace(valid, `"language":"zh"`, `"language":null`, 1), "key", false},
		{"unknown", strings.Replace(valid, `"language":"zh"`, `"language":"zh","operation":"audio.tts"`, 1), "key", false},
		{"short-preview", strings.Replace(valid, "这是一段符合长度约束的完整试听文本。", "试听", 1), "key", false},
		{"long-description", strings.Replace(valid, "沉稳的声音", strings.Repeat("声", 501), 1), "key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/voices", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.key != "" {
				c.Request.Header.Set("Idempotency-Key", tc.key)
			}
			request, err := service.ParseVoiceCreation(c)
			if tc.valid {
				require.NoError(t, err)
				assert.Len(t, request.Digest, 64)
			} else {
				assert.Error(t, err)
			}
		})
	}
	var digests []string
	for _, file := range [][]byte{[]byte("reference-bytes-a"), []byte("reference-bytes-b")} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for key, value := range map[string]string{"kind": "clone", "model": "cosyvoice-v3.5-plus", "language": "zh"} {
			require.NoError(t, writer.WriteField(key, value))
		}
		part, err := writer.CreateFormFile("file", "reference.wav")
		require.NoError(t, err)
		_, err = part.Write(file)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/voices", &body)
		c.Request.Header.Set("Content-Type", writer.FormDataContentType())
		c.Request.Header.Set("Idempotency-Key", "same-key")
		request, err := service.ParseVoiceCreation(c)
		require.NoError(t, err)
		assert.Equal(t, file, request.Reference)
		digests = append(digests, request.Digest)
	}
	assert.NotEqual(t, digests[0], digests[1], "相同幂等键的摘要必须覆盖原件字节")
}
