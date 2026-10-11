package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// EnforceRelayModelAccess是普通分流与持久音色读面的同一个组∩名单执行点。
func EnforceRelayModelAccess(c *gin.Context, name string) bool {
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		return true
	}
	value, exists := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
	allowed, valid := value.(map[string]bool)
	if !exists || !valid {
		abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenNoModelAccess))
		return false
	}
	if !allowed[ratio_setting.FormatMatchingModelName(name)] {
		abortWithOpenAiMessage(c, http.StatusForbidden, i18n.T(c, i18n.MsgDistributorTokenModelForbidden, map[string]any{"Model": name}))
		return false
	}
	return true
}

func PrepareVoiceResource() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
		var profile *model.VoiceProfile
		var err error
		if c.Request.Method == http.MethodPost {
			request, parseErr := service.ParseVoiceCreation(c)
			if parseErr != nil {
				service.VoiceHTTPError(c, http.StatusBadRequest, "invalid_request", parseErr.Error())
				return
			}
			c.Set("voice_create_request", request)
			c.Set("resolved_audio_voice_model", request.Model)
			profile, err = model.FindVoiceByIdempotencyKey(c.Request.Context(), userID, request.Key)
			if err == nil && profile.RequestDigest != request.Digest {
				service.VoiceHTTPError(c, http.StatusConflict, "voice_idempotency_conflict", "Idempotency-Key already belongs to a different request")
				return
			}
			if errors.Is(err, gorm.ErrRecordNotFound) {
				err = nil
			}
		} else if c.Param("voice_ref") != "" {
			profile, err = model.FindVoiceProfile(c.Request.Context(), userID, c.Param("voice_ref"))
		} else {
			key := c.Query("idempotency_key")
			if !service.ValidVoiceIdempotencyKey(key) || len(c.Request.URL.Query()) != 1 || len(c.Request.URL.Query()["idempotency_key"]) != 1 {
				service.VoiceHTTPError(c, http.StatusBadRequest, "invalid_request", "one idempotency_key is required")
				return
			}
			profile, err = model.FindVoiceByIdempotencyKey(c.Request.Context(), userID, key)
		}
		if err != nil {
			status, code := http.StatusServiceUnavailable, "voice_storage_unavailable"
			if errors.Is(err, gorm.ErrRecordNotFound) {
				status, code = http.StatusNotFound, "voice_unavailable"
			}
			service.VoiceHTTPError(c, status, code, code)
			return
		}
		if profile != nil {
			c.Set("voice_profile", profile)
			c.Set("resolved_audio_voice_model", profile.TargetModel)
		}
		if !EnforceRelayModelAccess(c, c.GetString("resolved_audio_voice_model")) {
			return
		}
		// 本地恢复、缺失供应商ID和试听不依赖在线渠道，仍执行同一模型约束。
		if profile != nil && (c.Request.Method == http.MethodPost || strings.HasSuffix(c.FullPath(), "/preview") || profile.ProviderVoiceId == nil || profile.State == model.VoiceStateDeleted || (c.Request.Method == http.MethodDelete && profile.DeleteDispatchedTime != nil)) {
			c.Next()
			return
		}
		if profile != nil && !pinVoiceChannel(c, int(profile.ChannelId)) {
			return
		}
		Distribute()(c)
	}
}

func pinVoiceChannel(c *gin.Context, channelID int) bool {
	constraints := service.GetChannelConstraints(c)
	if pin, found, _ := constraints.ResolvedPin(); found && pin.ChannelId != channelID {
		service.VoiceHTTPError(c, http.StatusForbidden, "voice_channel_mismatch", "voice is bound to a different channel")
		return false
	}
	constraints.AddPin(dto.ChannelPin{ChannelId: channelID, Source: dto.PinSourceOriginVoice, Rank: dto.PinRankOriginTask, RetryMode: dto.PinRetrySingleAttempt})
	return true
}

func PrepareVoiceSpeech() gin.HandlerFunc {
	return func(c *gin.Context) {
		var request kitdto.AudioRequest
		if err := common.UnmarshalBodyReusable(c, &request); err != nil {
			service.VoiceHTTPError(c, http.StatusBadRequest, "invalid_request", "invalid audio speech request")
			return
		}
		if model.ValidVoiceRef(request.Voice) {
			profile, err := model.ValidateVoiceSynthesis(c.Request.Context(), common.GetContextKeyInt(c, constant.ContextKeyUserId), request.Voice, request.Model)
			if err != nil {
				service.VoiceHTTPError(c, http.StatusBadRequest, "voice_unavailable", "voice is unavailable for this account and model")
				return
			}
			if !pinVoiceChannel(c, int(profile.ChannelId)) {
				return
			}
		}
		c.Next()
	}
}
