package service

import (
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/gin-gonic/gin"
)

var VoiceMedia *VoiceMediaStore

func InitVoiceMedia() error {
	store, err := NewVoiceMediaStore(common.GetEnvOrDefaultString("VOICE_MEDIA_DIR", "./data/voice-media"))
	if err != nil {
		return err
	}
	VoiceMedia = store
	go func() {
		for range time.NewTicker(time.Minute).C {
			if err := store.Cleanup(time.Now()); err != nil {
				logger.LogWarn(nil, "voice media cleanup failed: "+err.Error())
			}
		}
	}()
	return nil
}

func VoiceHTTPError(c *gin.Context, status int, code, message string) {
	if status >= http.StatusInternalServerError {
		logger.LogWarn(c, "voice request failed: "+code)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": "voice_error", "code": code}})
}
