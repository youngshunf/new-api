package ali

import (
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestQwenAudioTTSUsesDedicatedHTTPEndpoint(t *testing.T) {
	info := &relaycommon.RelayInfo{
		RelayMode: constant.RelayModeAudioSpeech,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "https://workspace.example.com",
			UpstreamModelName: "qwen-audio-3.0-tts-flash",
		},
	}

	requestURL, err := (&Adaptor{}).GetRequestURL(info)

	require.NoError(t, err)
	assert.Equal(
		t,
		"https://workspace.example.com/api/v1/services/audio/tts/SpeechSynthesizer",
		requestURL,
	)
}

func TestAliModelListIncludesValidatedSpeechModels(t *testing.T) {
	for _, model := range []string{
		"qwen3-asr-flash",
		"qwen3-tts-flash",
		"qwen3-tts-instruct-flash",
		"qwen-audio-3.0-tts-flash",
		"qwen-audio-3.0-tts-plus",
	} {
		assert.Contains(t, ModelList, model)
	}
}

func TestQwenAudioTTSRequestMatchesOfficialHTTPContract(t *testing.T) {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "qwen-audio-3.0-tts-flash"},
	}
	speed := 1.25

	body, err := ConvertAudioRequestForAli(c, info, dto.AudioRequest{
		Model:          "qwen-audio-3.0-tts-flash",
		Input:          "你好，唤星。",
		Voice:          "longanhuan_v3.6",
		Instructions:   "语气沉稳",
		ResponseFormat: "wav",
		Speed:          &speed,
	})

	require.NoError(t, err)
	encoded, err := io.ReadAll(body)
	require.NoError(t, err)
	assert.Equal(t, "qwen-audio-3.0-tts-flash", gjson.GetBytes(encoded, "model").String())
	assert.Equal(t, "你好，唤星。", gjson.GetBytes(encoded, "input.text").String())
	assert.Equal(t, "longanhuan_v3.6", gjson.GetBytes(encoded, "input.voice").String())
	assert.Equal(t, "语气沉稳", gjson.GetBytes(encoded, "input.instruction").String())
	assert.Equal(t, "wav", gjson.GetBytes(encoded, "input.format").String())
	assert.Equal(t, int64(24_000), gjson.GetBytes(encoded, "input.sample_rate").Int())
	assert.Equal(t, 1.25, gjson.GetBytes(encoded, "input.rate").Float())
	assert.False(t, gjson.GetBytes(encoded, "parameters").Exists())
}

func TestAliTTSErrorIncludesProviderRequestID(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"request_id":"provider-tts-1",
			"code":"InvalidParameter",
			"message":"voice is invalid",
			"output":{}
		}`)),
	}

	err, usage := AliTTSHandler(c, resp, &relaycommon.RelayInfo{})

	require.NotNil(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "InvalidParameter")
	assert.Contains(t, err.Error(), "provider-tts-1")
}

func TestAliTTSSuccessDownloadsAudioAndReturnsProviderUsage(t *testing.T) {
	audio, err := os.ReadFile("../../../tests/fixtures/voice-preview.wav")
	require.NoError(t, err)
	audioServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, http.MethodGet, request.Method)
		response.Header().Set("Content-Type", "audio/wav")
		_, err := response.Write(audio)
		require.NoError(t, err)
	}))
	t.Cleanup(audioServer.Close)
	parsed, parseErr := url.Parse(audioServer.URL)
	require.NoError(t, parseErr)
	fetch := system_setting.GetFetchSetting()
	previousFetch := *fetch
	fetch.AllowPrivateIp, fetch.AllowedPorts = true, []string{parsed.Port()}
	service.InitHttpClient()
	t.Cleanup(func() { *fetch = previousFetch; service.InitHttpClient() })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"request_id":"provider-tts-2",
			"output":{"audio":{"url":"` + audioServer.URL + `"}},
			"usage":{"characters":10}
		}`)),
	}

	err, rawUsage := AliTTSHandler(c, resp, &relaycommon.RelayInfo{})

	require.Nil(t, err)
	usage, ok := rawUsage.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, 10, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens)
	assert.Equal(t, 10, usage.TotalTokens)
	assert.Equal(t, "provider-tts-2", c.GetString(common.UpstreamRequestIdKey))
	assert.Equal(t, "audio/wav", recorder.Header().Get("Content-Type"))
	assert.Equal(t, audio, recorder.Body.Bytes())
}

func TestAliSTTErrorIncludesProviderRequestID(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`{
			"request_id":"provider-stt-1",
			"code":"InvalidParameter",
			"message":"audio is invalid"
		}`)),
	}

	usage, err := AliSTTHandler(c, resp, &relaycommon.RelayInfo{})

	require.NotNil(t, err)
	assert.Nil(t, usage)
	assert.Contains(t, err.Error(), "InvalidParameter")
	assert.Contains(t, err.Error(), "provider-stt-1")
}

// 网关发给上游的 input_audio 必须带 format（由上传文件扩展名或 Content-Type 推出）。
func TestAliSTTRequestForwardsAudioFormat(t *testing.T) {
	cases := []struct{ filename, contentType, want string }{
		{"a.wav", "audio/wav", "wav"},
		{"voice.MP3", "application/octet-stream", "mp3"},
		{"noext", "audio/mp4", "m4a"},
		{"clip.webm", "audio/webm", "webm"},
	}
	for _, tc := range cases {
		body := &strings.Builder{}
		mw := multipart.NewWriter(body)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="file"; filename="`+tc.filename+`"`)
		h.Set("Content-Type", tc.contentType)
		part, err := mw.CreatePart(h)
		require.NoError(t, err)
		_, _ = part.Write([]byte("RIFFxxxx"))
		require.NoError(t, mw.Close())

		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body.String()))
		c.Request.Header.Set("Content-Type", mw.FormDataContentType())

		r, err := ConvertAudioRequestForAliSTT(c, &relaycommon.RelayInfo{}, dto.AudioRequest{Model: "fun-asr-flash-2026-06-15"})
		require.NoError(t, err)
		raw, _ := io.ReadAll(r)
		assert.Equal(t, tc.want, gjson.GetBytes(raw, "messages.0.content.0.input_audio.format").String(), tc.filename)
	}
}
