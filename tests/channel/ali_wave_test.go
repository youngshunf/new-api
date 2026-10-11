package channeltests

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel/ali"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-audio/wav"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAliWAVBothAudioOutletsValidateLengths(t *testing.T) {
	header, err := os.ReadFile("../fixtures/ali-cosy-wave-header.bin")
	require.NoError(t, err)
	require.Len(t, header, 44)
	// 这是无语音的PCM结构向量，四个int16用于证伪重写头时损坏或增删数据。
	pcm := []byte{0, 0, 1, 0, 255, 127, 0, 128}
	observed := append(bytes.Clone(header), pcm...)
	standard := bytes.Clone(observed)
	binary.LittleEndian.PutUint32(standard[4:], uint32(len(standard)-8))
	binary.LittleEndian.PutUint32(standard[40:], uint32(len(pcm)))
	finiteMismatch := bytes.Clone(standard)
	binary.LittleEndian.PutUint32(finiteMismatch[4:], uint32(len(finiteMismatch)-6))
	binary.LittleEndian.PutUint32(finiteMismatch[40:], uint32(len(pcm)+2))
	dataMismatch := bytes.Clone(standard)
	binary.LittleEndian.PutUint32(dataMismatch[40:], uint32(len(pcm)+2))
	standardBadFormat := bytes.Clone(standard)
	binary.LittleEndian.PutUint16(standardBadFormat[20:], 3)
	standardMissingFormat := bytes.Clone(standard)
	copy(standardMissingFormat[12:16], "JUNK")
	badFormat := bytes.Clone(observed)
	binary.LittleEndian.PutUint16(badFormat[20:], 3)
	badAlignment := bytes.Clone(observed)
	binary.LittleEndian.PutUint16(badAlignment[32:], 4)
	badByteRate := bytes.Clone(observed)
	binary.LittleEndian.PutUint32(badByteRate[28:], 48001)
	unconfirmedMarker := bytes.Clone(observed)
	binary.LittleEndian.PutUint32(unconfirmedMarker[4:], 0xffffffff)
	binary.LittleEndian.PutUint32(unconfirmedMarker[40:], 0xffffffff)
	missingFormat := bytes.Clone(observed)
	copy(missingFormat[12:16], "JUNK")
	duplicateData := append(bytes.Clone(standard), []byte{'d', 'a', 't', 'a', 2, 0, 0, 0, 0, 0}...)
	binary.LittleEndian.PutUint32(duplicateData[4:], uint32(len(duplicateData)-8))

	cases := []struct {
		name  string
		audio []byte
		valid bool
	}{
		{"observed-placeholder", observed, true},
		{"standard-unchanged", standard, true},
		{"finite-mismatch", finiteMismatch, false},
		{"data-size-mismatch", dataMismatch, false},
		{"standard-bad-format", standardBadFormat, false},
		{"standard-missing-format", standardMissingFormat, false},
		{"truncated-standard", standard[:len(standard)-2], false},
		{"unaligned-placeholder", observed[:len(observed)-1], false},
		{"empty-placeholder", observed[:44], false},
		{"non-PCM-format", badFormat, false},
		{"bad-block-align", badAlignment, false},
		{"bad-byte-rate", badByteRate, false},
		{"unconfirmed-marker", unconfirmedMarker, false},
		{"missing-format", missingFormat, false},
		{"duplicate-data", duplicateData, false},
	}
	files := t.TempDir()
	for _, tc := range cases {
		require.NoError(t, os.WriteFile(filepath.Join(files, tc.name+".wav"), tc.audio, 0o600))
	}
	// 这里只启动真实HTTP文件服务验证下载消费者，不模拟供应商建档或计费成功。
	server := httptest.NewServer(http.FileServer(http.Dir(files)))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	fetch := system_setting.GetFetchSetting()
	previous := *fetch
	fetch.AllowPrivateIp, fetch.AllowedPorts = true, []string{parsed.Port()}
	service.InitHttpClient()
	t.Cleanup(func() { *fetch = previous; service.InitHttpClient() })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := common.Marshal(map[string]any{"request_id": "codec-only", "output": map[string]any{"audio": map[string]any{"url": server.URL + "/" + tc.name + ".wav"}}})
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", nil)
			apiErr, _ := ali.AliTTSHandler(context, &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(wire))}, &relaycommon.RelayInfo{})
			previewWire, err := common.Marshal(map[string]any{"request_id": "codec-only", "output": map[string]any{"voice_id": "codec-only", "preview_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(tc.audio), "sample_rate": 24000, "response_format": "wav"}}})
			require.NoError(t, err)
			result, err := ali.DecodeVoiceEnrollmentResponse(200, bytes.NewReader(previewWire), "create_voice", "cosyvoice-v3.5-plus", "")
			require.NoError(t, err)
			preview, previewErr := ali.DecodeVoicePreview(result)
			if !tc.valid {
				assert.NotNil(t, apiErr, "TTS不能转发异常WAV")
				assert.Empty(t, recorder.Body.Bytes(), "拒绝前不能写出成功音频")
				assert.Error(t, previewErr, "设计试听与TTS必须沿同一严格边界")
				return
			}
			require.Nil(t, apiErr)
			require.NoError(t, previewErr)
			assert.Equal(t, standard, recorder.Body.Bytes())
			assert.Equal(t, standard, preview)
			assert.Equal(t, pcm, preview[44:], "PCM不能因规范化而改变")
			decoder := wav.NewDecoder(bytes.NewReader(preview))
			require.True(t, decoder.IsValidFile())
			decoded, err := decoder.FullPCMBuffer()
			require.NoError(t, err)
			assert.Equal(t, []int{0, 1, 32767, -32768}, decoded.Data)
		})
	}
}
