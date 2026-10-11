package ali

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/go-audio/wav"
)

type VoiceEnrollmentInput struct {
	Action        string   `json:"action"`
	TargetModel   string   `json:"target_model,omitempty"`
	VoiceID       string   `json:"voice_id,omitempty"`
	Prefix        string   `json:"prefix,omitempty"`
	URL           string   `json:"url,omitempty"`
	VoicePrompt   string   `json:"voice_prompt,omitempty"`
	PreviewText   string   `json:"preview_text,omitempty"`
	LanguageHints []string `json:"language_hints,omitempty"`
}

type VoiceEnrollmentParameters struct {
	SampleRate     int    `json:"sample_rate"`
	ResponseFormat string `json:"response_format"`
}

type VoiceEnrollmentRequest struct {
	Model      string                     `json:"model"`
	Input      VoiceEnrollmentInput       `json:"input"`
	Parameters *VoiceEnrollmentParameters `json:"parameters,omitempty"`
}

type VoiceEnrollmentOutput struct {
	VoiceID      string `json:"voice_id"`
	TargetModel  string `json:"target_model"`
	Status       string `json:"status"`
	PreviewAudio *struct {
		Data           string `json:"data"`
		SampleRate     int    `json:"sample_rate"`
		ResponseFormat string `json:"response_format"`
	} `json:"preview_audio"`
}

type VoiceEnrollmentResult struct {
	RequestID string
	Output    VoiceEnrollmentOutput
}

// VoiceEnrollmentError只在明确的供应商4xx拒绝时可终结创建；网络或缺失回执继续unknown。
type VoiceEnrollmentError struct {
	Definitive bool
	Code       string
}

func (e *VoiceEnrollmentError) Error() string { return "voice enrollment failed: " + e.Code }

func SupportsVoiceEnrollment(model string) bool {
	// 这是已实现供应商协议范围，不自动发布库存或能力。
	switch model {
	case "cosyvoice-v3.5-plus", "cosyvoice-v3.5-flash", "cosyvoice-v3-plus", "cosyvoice-v3-flash":
		return true
	default:
		return false
	}
}

func NewVoiceEnrollmentCreation(kind, model, language, description, previewText, materialURL string) (VoiceEnrollmentRequest, error) {
	request := VoiceEnrollmentRequest{Model: "voice-enrollment", Input: VoiceEnrollmentInput{
		Action: "create_voice", TargetModel: model, Prefix: "hasn", LanguageHints: []string{language}}}
	if !SupportsVoiceEnrollment(model) || (language != "zh" && language != "en") {
		return request, errors.New("unsupported voice model or language")
	}
	switch kind {
	case "clone":
		if materialURL == "" || description != "" || previewText != "" {
			return request, errors.New("invalid voice clone request")
		}
		request.Input.URL = materialURL
	case "design":
		if !utf8.ValidString(description) || !utf8.ValidString(previewText) || utf8.RuneCountInString(description) < 1 || utf8.RuneCountInString(description) > 500 || utf8.RuneCountInString(previewText) < 15 || utf8.RuneCountInString(previewText) > 200 || materialURL != "" {
			return request, errors.New("invalid voice design request")
		}
		request.Input.VoicePrompt, request.Input.PreviewText = description, previewText
		request.Parameters = &VoiceEnrollmentParameters{SampleRate: 24000, ResponseFormat: "wav"}
	default:
		return request, errors.New("invalid voice kind")
	}
	return request, nil
}

type voiceEnrollmentAdaptor struct{ Adaptor }

func (a *voiceEnrollmentAdaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return strings.TrimRight(info.ChannelBaseUrl, "/") + "/api/v1/services/audio/tts/customization", nil
}

// CallVoiceEnrollment共用渠道的HeaderOverride、代理和派发观测；无任何创建重试。
func CallVoiceEnrollment(c *gin.Context, info *relaycommon.RelayInfo, request VoiceEnrollmentRequest) (*VoiceEnrollmentResult, error) {
	body, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	upstream := c.Copy()
	upstream.Request = c.Request.Clone(ctx)
	upstream.Request.Method = http.MethodPost
	upstream.Request.Body = http.NoBody
	upstream.Request.Header.Set("Content-Type", "application/json")
	copyInfo := *info
	copyInfo.RelayMode, copyInfo.IsStream = relayconstant.RelayModeAudioSpeech, false
	response, err := channel.DoApiRequest(&voiceEnrollmentAdaptor{}, upstream, &copyInfo, bytes.NewReader(body))
	if err != nil {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	defer response.Body.Close()
	result, err := DecodeVoiceEnrollmentResponse(response.StatusCode, response.Body, request.Input.Action, request.Input.TargetModel, request.Input.VoiceID)
	if result != nil {
		c.Set(common.UpstreamRequestIdKey, result.RequestID)
	}
	return result, err
}

func DecodeVoiceEnrollmentResponse(status int, body io.Reader, action, targetModel, voiceID string) (*VoiceEnrollmentResult, error) {
	data, err := io.ReadAll(io.LimitReader(body, 12*1024*1024+1))
	if err != nil || len(data) > 12*1024*1024 {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	var response struct {
		RequestID string          `json:"request_id"`
		Output    json.RawMessage `json:"output"`
		Code      string          `json:"code"`
	}
	if err = common.Unmarshal(data, &response); err != nil {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	if status < 200 || status >= 300 || response.Code != "" {
		return nil, &VoiceEnrollmentError{Definitive: status >= 400 && status < 500 && response.Code != "" && response.RequestID != "", Code: "provider_rejected"}
	}
	if response.RequestID == "" || len(response.Output) == 0 || string(response.Output) == "null" {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	result := &VoiceEnrollmentResult{RequestID: response.RequestID}
	if err = common.Unmarshal(response.Output, &result.Output); err != nil {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	valid := false
	switch action {
	case "create_voice":
		valid = result.Output.VoiceID != "" && utf8.RuneCountInString(result.Output.VoiceID) <= 256 && (result.Output.TargetModel == "" || result.Output.TargetModel == targetModel)
	case "query_voice":
		valid = result.Output.TargetModel == targetModel && (result.Output.VoiceID == "" || result.Output.VoiceID == voiceID) &&
			(result.Output.Status == "OK" || result.Output.Status == "DEPLOYING" || result.Output.Status == "UNDEPLOYED")
	case "delete_voice":
		var output map[string]any
		valid = common.Unmarshal(response.Output, &output) == nil && output != nil && len(output) == 0
	}
	if !valid {
		return nil, &VoiceEnrollmentError{Code: "provider_response_unknown"}
	}
	return result, nil
}

func DecodeVoicePreview(result *VoiceEnrollmentResult) ([]byte, error) {
	if result == nil || result.Output.PreviewAudio == nil {
		return nil, errors.New("voice_preview_unavailable")
	}
	preview := result.Output.PreviewAudio
	if preview.ResponseFormat != "wav" || preview.SampleRate != 24000 || len(preview.Data) > (service.MaximumVoicePreviewBytes+2)/3*4 {
		return nil, errors.New("invalid voice preview format or size")
	}
	data, err := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(preview.Data)), service.MaximumVoicePreviewBytes+1))
	if err != nil || len(data) > service.MaximumVoicePreviewBytes || len(data) < 44 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("invalid voice preview audio")
	}
	decoder := wav.NewDecoder(bytes.NewReader(data))
	if !decoder.IsValidFile() || decoder.SampleRate != 24000 {
		return nil, errors.New("invalid voice preview WAV")
	}
	pcm, err := decoder.FullPCMBuffer()
	if err != nil || pcm == nil || len(pcm.Data) == 0 {
		return nil, errors.New("invalid voice preview PCM")
	}
	return data, nil
}
