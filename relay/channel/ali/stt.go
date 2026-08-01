package ali

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

// ── DashScope STT (qwen3-asr) via Chat Completions ─────────────────────────────

type AliSTTInputAudio struct {
	Data string `json:"data"`
}

type AliSTTChatMessageContent struct {
	Type       string            `json:"type"`
	InputAudio *AliSTTInputAudio `json:"input_audio,omitempty"`
}

type AliSTTMessage struct {
	Role    string                     `json:"role"`
	Content []AliSTTChatMessageContent `json:"content"`
}

type AliSTTASROptions struct {
	EnableITN bool `json:"enable_itn"`
}

type AliSTTRequest struct {
	Model      string           `json:"model"`
	Messages   []AliSTTMessage  `json:"messages"`
	Stream     bool             `json:"stream"`
	ASROptions AliSTTASROptions `json:"asr_options"`
}

type AliSTTResponse struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// ConvertAudioRequestForAliSTT converts an OpenAI multipart AudioRequest to Ali qwen3-asr chat format.
func ConvertAudioRequestForAliSTT(c *gin.Context, info *relaycommon.RelayInfo, req dto.AudioRequest) (io.Reader, error) {
	// Ensure multipart form is parsed
	if c.Request.MultipartForm == nil {
		if err := c.Request.ParseMultipartForm(1024 * 1024 * 100); err != nil {
			return nil, fmt.Errorf("failed to parse multipart form: %w", err)
		}
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		return nil, fmt.Errorf("failed to get audio file from form: %w", err)
	}
	defer file.Close()

	audioData, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio file: %w", err)
	}

	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/wav"
	}

	encodedData := base64.StdEncoding.EncodeToString(audioData)
	dataURI := fmt.Sprintf("data:%s;base64,%s", contentType, encodedData)

	aliReq := AliSTTRequest{
		Model: req.Model,
		Messages: []AliSTTMessage{
			{
				Role: "user",
				Content: []AliSTTChatMessageContent{
					{
						Type: "input_audio",
						InputAudio: &AliSTTInputAudio{
							Data: dataURI,
						},
					},
				},
			},
		},
		Stream: false,
		ASROptions: AliSTTASROptions{
			EnableITN: false, // matches user example
		},
	}

	jsonData, err := json.Marshal(aliReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Ali STT request: %w", err)
	}

	// Set content type to application/json for the downstream request
	c.Request.Header.Set("Content-Type", "application/json")
	return bytes.NewReader(jsonData), nil
}

// AliSTTHandler processes the DashScope chat completions response and returns an OpenAI STT response.
func AliSTTHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewError(
			fmt.Errorf("failed to read Ali STT response: %w", err),
			types.ErrorCodeReadResponseBodyFailed,
		)
	}

	var chatResp AliSTTResponse
	if err := common.Unmarshal(body, &chatResp); err != nil {
		// Log raw response for debugging
		return nil, types.NewError(
			fmt.Errorf("failed to parse Ali STT response. Raw: %s, Error: %w", string(body), err),
			types.ErrorCodeReadResponseBodyFailed,
		)
	}

	if chatResp.Code != "" || chatResp.Message != "" {
		return nil, types.NewError(
			fmt.Errorf("DashScope STT error: %s (%s)", chatResp.Message, chatResp.Code),
			types.ErrorCodeDoRequestFailed,
		)
	}

	if len(chatResp.Choices) == 0 {
		return nil, types.NewError(
			fmt.Errorf("DashScope STT returned no choices. Raw: %s", string(body)),
			types.ErrorCodeDoRequestFailed,
		)
	}

	text := chatResp.Choices[0].Message.Content

	// OpenAI compatible STT format
	sttResp := dto.AudioResponse{
		Text: text,
	}

	// Output as JSON
	c.Header("Content-Type", "application/json")
	c.Writer.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(c.Writer).Encode(sttResp); err != nil {
		return nil, types.NewError(
			fmt.Errorf("failed to encode STT response: %w", err),
			types.ErrorCodeJsonMarshalFailed,
		)
	}

	usage := &dto.Usage{
		PromptTokens:     chatResp.Usage.PromptTokens,
		CompletionTokens: chatResp.Usage.CompletionTokens,
		TotalTokens:      chatResp.Usage.TotalTokens,
	}

	return usage, nil
}
