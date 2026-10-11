package channeltests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/ali"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAliVoicePreviewDecodesActualPCMAndRejectsInvalidAudio(t *testing.T) {
	wave := make([]byte, 46)
	copy(wave, "RIFF")
	binary.LittleEndian.PutUint32(wave[4:], 38)
	copy(wave[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wave[16:], 16)
	binary.LittleEndian.PutUint16(wave[20:], 1)
	binary.LittleEndian.PutUint16(wave[22:], 1)
	binary.LittleEndian.PutUint32(wave[24:], 24000)
	binary.LittleEndian.PutUint32(wave[28:], 48000)
	binary.LittleEndian.PutUint16(wave[32:], 2)
	binary.LittleEndian.PutUint16(wave[34:], 16)
	copy(wave[36:], "data")
	binary.LittleEndian.PutUint32(wave[40:], 2)
	binary.LittleEndian.PutUint16(wave[44:], 1234)
	for _, tc := range []struct {
		name, encoded string
		valid         bool
	}{
		{"PCM", base64.StdEncoding.EncodeToString(wave), true},
		{"invalid-base64", "%%%", false},
		{"invalid-WAV", base64.StdEncoding.EncodeToString([]byte("RIFF-broken")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := common.Marshal(map[string]any{"request_id": "codec-unit", "output": map[string]any{"voice_id": "voice1", "preview_audio": map[string]any{"data": tc.encoded, "response_format": "wav", "sample_rate": 24000}}})
			require.NoError(t, err)
			result, err := ali.DecodeVoiceEnrollmentResponse(200, bytes.NewReader(body), "create_voice", "cosyvoice-v3.5-plus", "")
			require.NoError(t, err)
			decoded, err := ali.DecodeVoicePreview(result)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, wave, decoded)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestAliVoiceEnrollmentSingleDispatchOnTransportFault(t *testing.T) {
	service.InitHttpClient()
	for _, fault := range []string{"close", "307", "308"} {
		t.Run(fault, func(t *testing.T) {
			wire := newFaultWire(t, "h1", fault)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/audio/voices/01998dd2-0000-7000-8000-000000007701", nil).WithContext(ctx)
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelBaseUrl: wire.target(), ChannelSetting: dto.ChannelSettings{HTTPProtocol: dto.HTTPProtocolHTTP1}},
				UseRuntimeHeadersOverride: true, RuntimeHeadersOverride: map[string]interface{}{"Idempotency-Key": "must-not-replay"}}
			_, err := ali.CallVoiceEnrollment(c, info, ali.VoiceEnrollmentRequest{Model: "voice-enrollment", Input: ali.VoiceEnrollmentInput{Action: "query_voice", VoiceID: "provider-voice"}})
			require.Error(t, err)
			observations := wire.observations(t)
			require.Len(t, observations, 1, "真实断连与重定向不能重派")
			assert.Equal(t, http.MethodPost, observations[0].method, "供应商query也使用POST")
			assert.Empty(t, observations[0].headers.Get("Idempotency-Key"))
			assert.Equal(t, http.MethodGet, c.Request.Method, "不能改写公共入口方法")
		})
	}
}

func TestAliCosyVoiceHTTPUsesNativeSpeech(t *testing.T) {
	info := &relaycommon.RelayInfo{RelayMode: relayconstant.RelayModeAudioSpeech, ChannelMeta: &relaycommon.ChannelMeta{}}
	info.UpstreamModelName, info.ChannelBaseUrl = "cosyvoice-v3.5-plus", "https://dashscope.aliyuncs.com"
	url, err := (&ali.Adaptor{}).GetRequestURL(info)
	require.NoError(t, err)
	assert.Equal(t, "https://dashscope.aliyuncs.com/api/v1/services/audio/tts/SpeechSynthesizer", url)
	speed := 0.0
	reader, err := ali.ConvertAudioRequestForAli(nil, info, dto.AudioRequest{Model: "cosyvoice-v3.5-plus", Input: "固定试听文本", Voice: "native-provider-voice", ResponseFormat: "wav", Speed: &speed})
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, common.Unmarshal(data, &wire))
	input, ok := wire["input"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "native-provider-voice", input["voice"])
	assert.Equal(t, "wav", input["format"])
	assert.Equal(t, float64(24000), input["sample_rate"])
	assert.Equal(t, float64(0), input["rate"], "显式零值不能静默丢失")
}

func TestAliVoiceEnrollmentWireAndAckValidation(t *testing.T) {
	clone, err := ali.NewVoiceEnrollmentCreation("clone", "cosyvoice-v3.5-plus", "zh", "", "", "https://gateway.example/v1/audio/voice-materials/token")
	require.NoError(t, err)
	assert.Equal(t, "voice-enrollment", clone.Model)
	assert.Equal(t, "create_voice", clone.Input.Action)
	assert.Equal(t, "cosyvoice-v3.5-plus", clone.Input.TargetModel)
	assert.Nil(t, clone.Parameters)
	design, err := ali.NewVoiceEnrollmentCreation("design", "cosyvoice-v3.5-plus", "en", "A calm adult voice", "This is a complete preview sentence.", "")
	require.NoError(t, err)
	assert.Equal(t, "A calm adult voice", design.Input.VoicePrompt)
	require.NotNil(t, design.Parameters)
	assert.Equal(t, "wav", design.Parameters.ResponseFormat)
	_, err = ali.NewVoiceEnrollmentCreation("design", "qwen3-tts-flash", "zh", "声音描述", "这是一段符合长度要求的试听文本。", "")
	assert.Error(t, err, "不能拿不支持建档的TTS模型代替")
	for _, tc := range []struct {
		name, action, body string
		status             int
		valid              bool
	}{
		{"create-id", "create_voice", `{"request_id":"r1","output":{"voice_id":"voice1"}}`, 200, true},
		{"missing-id", "create_voice", `{"request_id":"r1","output":{}}`, 200, false},
		{"missing-ack", "delete_voice", `{"output":{}}`, 200, false},
		{"null-output", "delete_voice", `{"request_id":"r1","output":null}`, 200, false},
		{"delete-ack", "delete_voice", `{"request_id":"r1","output":{}}`, 200, true},
		{"404-not-delete-ack", "delete_voice", `{"request_id":"r1","code":"NotFound","output":{}}`, 404, false},
		{"query-target", "query_voice", `{"request_id":"r1","output":{"target_model":"cosyvoice-v3.5-plus","status":"OK"}}`, 200, true},
		{"query-wrong-model", "query_voice", `{"request_id":"r1","output":{"target_model":"other","status":"OK"}}`, 200, false},
		{"query-unknown-status", "query_voice", `{"request_id":"r1","output":{"target_model":"cosyvoice-v3.5-plus","status":"NEW"}}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ali.DecodeVoiceEnrollmentResponse(tc.status, strings.NewReader(tc.body), tc.action, "cosyvoice-v3.5-plus", "voice1")
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
