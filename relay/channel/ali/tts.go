package ali

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// AliTTSInput represents the input field of a qwen3-tts request.
type AliTTSInput struct {
	Text         string `json:"text"`
	Voice        string `json:"voice,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// AliTTSParameters represents the parameters field of a qwen3-tts request.
type AliTTSParameters struct {
	Format string `json:"format,omitempty"`
}

// AliTTSRequest represents a DashScope qwen3-tts multimodal-generation request.
type AliTTSRequest struct {
	Model      string            `json:"model"`
	Input      AliTTSInput       `json:"input"`
	Parameters *AliTTSParameters `json:"parameters,omitempty"`
}

type AliQwenAudioTTSInput struct {
	Text        string   `json:"text"`
	Voice       string   `json:"voice"`
	Format      string   `json:"format,omitempty"`
	SampleRate  int      `json:"sample_rate,omitempty"`
	Rate        *float64 `json:"rate,omitempty"`
	Instruction string   `json:"instruction,omitempty"`
}

type AliQwenAudioTTSRequest struct {
	Model string               `json:"model"`
	Input AliQwenAudioTTSInput `json:"input"`
}

// ── DashScope qwen3-tts 响应结构 ─────────────────────────────

// AliTTSAudio represents an audio item in the TTS output.
type AliTTSAudio struct {
	URL string `json:"url,omitempty"`
}

// AliTTSOutput represents the output field of a qwen3-tts response.
type AliTTSOutput struct {
	Audio AliTTSAudio `json:"audio,omitempty"`
}

// AliTTSUsage represents the usage field of the TTS response.
type AliTTSUsage struct {
	Characters int `json:"characters,omitempty"`
}

// AliTTSResponse represents a DashScope qwen3-tts response.
type AliTTSResponse struct {
	RequestID string       `json:"request_id,omitempty"`
	Output    AliTTSOutput `json:"output"`
	Usage     AliTTSUsage  `json:"usage,omitempty"`
	Code      string       `json:"code,omitempty"`
	Message   string       `json:"message,omitempty"`
}

// ── 转换函数 ─────────────────────────────────────────────────

// ConvertOpenAITTSToAliTTS converts an OpenAI AudioRequest to Ali qwen3-tts format.
//
// Mapping:
//   - model            → model
//   - input (text)     → input.text
//   - instructions     → input.instructions
//   - voice            → input.voice
//   - response_format  → parameters.format
func ConvertOpenAITTSToAliTTS(req dto.AudioRequest) AliTTSRequest {
	input := AliTTSInput{
		Text:         req.Input,
		Voice:        req.Voice,
		Instructions: req.Instructions,
	}

	var params *AliTTSParameters
	if req.ResponseFormat != "" {
		params = &AliTTSParameters{
			Format: req.ResponseFormat,
		}
	}

	return AliTTSRequest{
		Model:      req.Model,
		Input:      input,
		Parameters: params,
	}
}

// ConvertAudioRequestForAli converts an OpenAI AudioRequest to a DashScope qwen3-tts
// request body (io.Reader). Only TTS (speech) mode is supported.
func ConvertAudioRequestForAli(c *gin.Context, info *relaycommon.RelayInfo, req dto.AudioRequest) (io.Reader, error) {
	var aliReq any = ConvertOpenAITTSToAliTTS(req)
	upstreamModel := req.Model
	if info != nil && info.UpstreamModelName != "" {
		upstreamModel = info.UpstreamModelName
	}
	if isAliQwenAudioTTSModel(upstreamModel) {
		aliReq = AliQwenAudioTTSRequest{
			Model: req.Model,
			Input: AliQwenAudioTTSInput{
				Text:        req.Input,
				Voice:       req.Voice,
				Format:      req.ResponseFormat,
				SampleRate:  24_000,
				Rate:        req.Speed,
				Instruction: req.Instructions,
			},
		}
	}
	jsonData, err := json.Marshal(aliReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Ali TTS request: %w", err)
	}
	return bytes.NewReader(jsonData), nil
}

// ── TTS 响应处理 ─────────────────────────────────────────────

// AliTTSHandler processes the DashScope qwen3-tts response.
//
// DashScope returns a JSON response containing an audio URL, not raw audio bytes.
// This handler downloads the audio from the URL and streams it back to the client,
// making the response compatible with OpenAI TTS clients that expect raw audio data.
func AliTTSHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (*types.NewAPIError, any) {
	defer service.CloseResponseBodyGracefully(resp)

	// Read the JSON response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewError(
			fmt.Errorf("failed to read Ali TTS response: %w", err),
			types.ErrorCodeReadResponseBodyFailed,
		), nil
	}

	var aliResp AliTTSResponse
	if err := common.Unmarshal(body, &aliResp); err != nil {
		return types.NewError(
			fmt.Errorf("failed to parse Ali TTS response: %w", err),
			types.ErrorCodeReadResponseBodyFailed,
		), nil
	}

	// Check for API errors
	if aliResp.Code != "" {
		providerRequestID := aliProviderRequestID(c, aliResp.RequestID)
		return types.NewError(
			fmt.Errorf(
				"DashScope TTS error (%s): %s, provider request id: %s",
				aliResp.Code,
				aliResp.Message,
				providerRequestID,
			),
			types.ErrorCodeDoRequestFailed,
		), nil
	}

	if aliResp.Output.Audio.URL == "" {
		return types.NewError(
			fmt.Errorf("DashScope TTS returned no audio URL. Raw: %s", string(body)),
			types.ErrorCodeDoRequestFailed,
		), nil
	}

	audioURL := aliResp.Output.Audio.URL
	providerRequestID := aliProviderRequestID(c, aliResp.RequestID)
	logger.LogInfo(c, fmt.Sprintf("Ali TTS: downloading audio, provider request id: %s", providerRequestID))

	downloadRequest, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, audioURL, nil)
	if err != nil {
		return types.NewError(
			fmt.Errorf("failed to create TTS audio download request: %w", err),
			types.ErrorCodeDoRequestFailed,
		), nil
	}
	httpClient := &http.Client{Timeout: 2 * time.Minute}
	audioResp, err := httpClient.Do(downloadRequest)
	if err != nil {
		return types.NewError(
			fmt.Errorf("failed to download TTS audio: %w", err),
			types.ErrorCodeDoRequestFailed,
		), nil
	}
	defer audioResp.Body.Close()

	if audioResp.StatusCode != http.StatusOK {
		return types.NewError(
			fmt.Errorf("TTS audio download failed: status %d", audioResp.StatusCode),
			types.ErrorCodeDoRequestFailed,
		), nil
	}

	const maximumAudioBytes = 64 * 1024 * 1024
	audio, err := io.ReadAll(io.LimitReader(audioResp.Body, maximumAudioBytes+1))
	if err != nil {
		return types.NewError(
			fmt.Errorf("failed to read TTS audio download: %w", err),
			types.ErrorCodeReadResponseBodyFailed,
		), nil
	}
	if len(audio) > maximumAudioBytes {
		return types.NewError(
			fmt.Errorf("TTS audio download exceeds %d bytes", maximumAudioBytes),
			types.ErrorCodeBadResponseBody,
		), nil
	}

	contentType := audioResp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/wav"
	}

	c.Writer.Header().Set("Content-Type", contentType)
	c.Writer.WriteHeader(http.StatusOK)
	if _, err := c.Writer.Write(audio); err != nil {
		return types.NewError(
			fmt.Errorf("failed to write TTS audio response: %w", err),
			types.ErrorCodeDoRequestFailed,
		), nil
	}

	usage := &dto.Usage{
		PromptTokens: aliResp.Usage.Characters,
		TotalTokens:  aliResp.Usage.Characters,
	}

	return nil, usage
}
