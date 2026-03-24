package ali

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
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
	RequestID string      `json:"request_id,omitempty"`
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
	aliReq := ConvertOpenAITTSToAliTTS(req)
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
		return types.NewError(
			fmt.Errorf("DashScope TTS error (%s): %s", aliResp.Code, aliResp.Message),
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
	logger.LogInfo(c, fmt.Sprintf("Ali TTS: redirecting to audio URL %s", audioURL))

	// Directly redirect the client to the audio URL instead of streaming it, 
	// saving bandwidth and matching minimax behavior
	c.Redirect(http.StatusFound, audioURL)

	// Build usage based on characters for billing
	usage := &dto.Usage{
		PromptTokens:     aliResp.Usage.Characters,
		CompletionTokens: 0,
		TotalTokens:      aliResp.Usage.Characters,
	}

	return nil, usage
}
