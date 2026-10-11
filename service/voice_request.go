package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
)

func ParseVoiceCreation(c *gin.Context) (*dto.VoiceCreateRequest, error) {
	request := &dto.VoiceCreateRequest{Key: c.GetHeader("Idempotency-Key")}
	if len(c.Request.Header.Values("Idempotency-Key")) != 1 || !ValidVoiceIdempotencyKey(request.Key) {
		return nil, errors.New("Idempotency-Key is required and must contain 1 to 128 visible ASCII characters")
	}
	contentType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		return nil, errors.New("invalid Content-Type")
	}
	switch contentType {
	case "application/json":
		data, err := io.ReadAll(io.LimitReader(c.Request.Body, 8193))
		if err != nil || len(data) > 8192 {
			return nil, errors.New("voice design request exceeds size limit")
		}
		if err = common.UnmarshalStrictObject(data, request); err != nil {
			return nil, err
		}
		var fields map[string]any
		if err = common.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		for field := range fields {
			if field != "kind" && field != "model" && field != "language" && field != "description" && field != "preview_text" {
				return nil, errors.New("unknown voice design field")
			}
		}
		request.Key = c.GetHeader("Idempotency-Key")
		if request.Kind != "design" {
			return nil, errors.New("JSON voice creation requires kind=design")
		}
	case "multipart/form-data":
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaximumVoiceMaterialBytes+64*1024)
		reader, err := c.Request.MultipartReader()
		if err != nil {
			return nil, err
		}
		fields := map[string]string{}
		seen := map[string]bool{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			name := part.FormName()
			if seen[name] || (name != "kind" && name != "model" && name != "language" && name != "file") {
				_ = part.Close()
				return nil, errors.New("unknown or duplicate voice clone field")
			}
			seen[name] = true
			limit := int64(1024)
			if name == "file" {
				limit = MaximumVoiceMaterialBytes
			}
			data, readErr := io.ReadAll(io.LimitReader(part, limit+1))
			_ = part.Close()
			if readErr != nil || len(data) == 0 || int64(len(data)) > limit {
				return nil, errors.New("voice clone field size is outside the allowed range")
			}
			if name == "file" {
				request.Reference = data
			} else {
				fields[name] = string(data)
			}
		}
		request.Kind, request.Model, request.Language = fields["kind"], fields["model"], fields["language"]
		if request.Kind != "clone" || len(request.Reference) == 0 {
			return nil, errors.New("multipart voice creation requires kind=clone and file")
		}
	default:
		return nil, errors.New("voice creation requires JSON or multipart")
	}
	if request.Model == "" || len(request.Model) > 256 || strings.TrimSpace(request.Model) != request.Model || !utf8.ValidString(request.Model) || (request.Language != "zh" && request.Language != "en") {
		return nil, errors.New("invalid voice model or language")
	}
	if request.Kind == "design" && (!utf8.ValidString(request.Description) || !utf8.ValidString(request.PreviewText) || utf8.RuneCountInString(request.Description) < 1 || utf8.RuneCountInString(request.Description) > 500 || utf8.RuneCountInString(request.PreviewText) < 15 || utf8.RuneCountInString(request.PreviewText) > 200) {
		return nil, errors.New("voice description must contain 1 to 500 characters and preview_text 15 to 200 characters")
	}
	summary := struct {
		Kind, Model, Language, Description, PreviewText, ReferenceDigest string
	}{Kind: request.Kind, Model: request.Model, Language: request.Language, Description: request.Description, PreviewText: request.PreviewText}
	if request.Reference != nil {
		summary.ReferenceDigest = fmt.Sprintf("%x", sha256.Sum256(request.Reference))
	}
	canonical, err := common.Marshal(summary)
	if err != nil {
		return nil, err
	}
	request.Digest = fmt.Sprintf("%x", sha256.Sum256(canonical))
	return request, nil
}

func ValidVoiceIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 {
		return false
	}
	for _, value := range key {
		if value < 33 || value > 126 {
			return false
		}
	}
	return true
}
