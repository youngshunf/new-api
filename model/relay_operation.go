package model

import "errors"

// IsRelayOperation 接受与通用模型用途合同一致的闭集，不为Speech另开purpose。
func IsRelayOperation(operation string) bool {
	switch operation {
	case "chat.completions", "vision", "responses", "messages", "embedding", "rerank", "image.generate", "image.edit",
		"audio.transcribe", "audio.translate", "audio.tts", "video.generate", "audio.voice_create", "audio.voice_query", "audio.voice_delete":
		return true
	default:
		return false
	}
}

// ValidateRelayLeaseOperation 在控制器和持久入口共用Runtime/call与历史音频边界。
func ValidateRelayLeaseOperation(operation, purpose string, models []string) error {
	if operation != "" {
		if purpose != "generic" || !IsRelayOperation(operation) || len(models) != 1 {
			return errors.New("ordinary call requires a known operation, generic/legacy and exactly one model")
		}
		return nil
	}
	if purpose == "generic" && len(models) != 0 {
		return errors.New("Runtime relay lease requires an empty model_limits; ordinary call must include operation")
	}
	if purpose != "generic" && len(models) != 1 {
		return errors.New("managed audio lease requires exactly one model")
	}
	return nil
}
