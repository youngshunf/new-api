package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/channel/ali"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

func writeVoiceResource(c *gin.Context, profile *model.VoiceProfile) {
	preview := service.VoiceMedia != nil && profile.State != model.VoiceStateDeleted && profile.DeleteRequestedTime == nil && service.VoiceMedia.HasPreview(profile.VoiceRef)
	c.JSON(http.StatusOK, dto.VoiceResource{VoiceRef: profile.VoiceRef, Kind: profile.Kind, Model: profile.TargetModel,
		State: profile.State, FailureReason: profile.FailureReason, PreviewAvailable: preview, RetentionPolicy: profile.RetentionPolicy,
		DeleteSupported: true, DeletePending: profile.DeleteRequestedTime != nil && profile.State != model.VoiceStateDeleted})
}

func voiceRelayInfo(c *gin.Context, targetModel string) (*relaycommon.RelayInfo, bool) {
	if common.GetContextKeyInt(c, constant.ContextKeyChannelType) != constant.ChannelTypeAli || !ali.SupportsVoiceEnrollment(targetModel) {
		service.VoiceHTTPError(c, http.StatusBadRequest, "voice_backend_unsupported", "selected channel does not implement this voice model")
		return nil, false
	}
	info := relaycommon.GenRelayInfoOpenAIAudio(c, &kitdto.BaseRequest{})
	info.InitChannelMeta(c)
	return info, true
}

func CreateVoice(c *gin.Context) {
	if existing, ok := c.Get("voice_profile"); ok {
		writeVoiceResource(c, existing.(*model.VoiceProfile))
		return
	}
	request := c.MustGet("voice_create_request").(*dto.VoiceCreateRequest)
	info, supported := voiceRelayInfo(c, request.Model)
	if !supported {
		return
	}
	if service.VoiceMedia == nil {
		service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_media_unavailable", "voice media store is unavailable")
		return
	}
	publicAddress := ""
	if request.Kind == "clone" {
		publicAddress = strings.TrimRight(common.GetEnvOrDefaultString("VOICE_PUBLIC_ADDRESS", ""), "/")
		parsed, err := url.Parse(publicAddress)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_material_address_unavailable", "VOICE_PUBLIC_ADDRESS must be a trusted public HTTPS origin")
			return
		}
	}
	info.BillingModelName = "voice-enrollment"
	groupRatio := helper.HandleGroupRatio(c, info)
	info.PriceData = hosttypes.PriceData{UsePrice: true, FreeModel: true, ModelPrice: 0, GroupRatioInfo: groupRatio}
	if _, configured := ratio_setting.GetModelPrice("voice-enrollment", false); configured {
		priceInfo := *info
		priceInfo.OriginModelName = "voice-enrollment"
		price, err := helper.ModelPriceHelperPerCall(c, &priceInfo)
		if err != nil {
			service.VoiceHTTPError(c, http.StatusBadRequest, "voice_price_invalid", "voice-enrollment price is invalid")
			return
		}
		info.PriceData = price
	}
	// 免费建档也沿原BillingSession核账户与额度，未配置免费仅适用于已核CosyVoice。
	if apiErr := service.PreConsumeBilling(c, info.PriceData.Quota, info); apiErr != nil {
		service.VoiceHTTPError(c, apiErr.StatusCode, "voice_billing_rejected", apiErr.Error())
		return
	}
	defer info.Billing.Refund(c)
	profile, dispatch, err := model.BeginVoiceCreation(c.Request.Context(), model.VoiceCreationSpec{
		UserId: info.UserId, CreatorTokenId: info.TokenId, ChannelId: info.ChannelId, IdempotencyKey: request.Key,
		RequestDigest: request.Digest, Kind: request.Kind, Language: request.Language, TargetModel: request.Model})
	if err != nil {
		status, code := http.StatusServiceUnavailable, "voice_storage_unavailable"
		if errors.Is(err, model.ErrVoiceIdempotencyConflict) {
			status, code = http.StatusConflict, "voice_idempotency_conflict"
		}
		service.VoiceHTTPError(c, status, code, code)
		return
	}
	if !dispatch {
		writeVoiceResource(c, profile)
		return
	}
	var materialToken, materialURL string
	if request.Kind == "clone" {
		materialToken, err = service.VoiceMedia.SaveMaterial(bytes.NewReader(request.Reference))
		if err == nil {
			materialURL = publicAddress + "/v1/audio/voice-materials/" + materialToken
			defer func() {
				if cleanupErr := service.VoiceMedia.RemoveMaterial(materialToken); cleanupErr != nil {
					logger.LogWarn(c, "voice material removal failed: "+cleanupErr.Error())
				}
			}()
		}
	}
	var providerRequest ali.VoiceEnrollmentRequest
	if err == nil {
		providerRequest, err = ali.NewVoiceEnrollmentCreation(request.Kind, request.Model, request.Language, request.Description, request.PreviewText, materialURL)
	}
	if err != nil {
		updated, recordErr := model.RecordVoiceFailure(c.Request.Context(), info.UserId, profile.VoiceRef, "voice_material_unavailable", true)
		if recordErr != nil {
			service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist voice creation failure")
			return
		}
		writeVoiceResource(c, updated)
		return
	}
	result, providerErr := ali.CallVoiceEnrollment(c, info, providerRequest)
	// 回执落库不依赖客户端仍在线；超时有界，进程退出前不把真实ID丢成可重建状态。
	commitContext, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	if providerErr != nil {
		definitive := false
		reason := "provider_response_unknown"
		var rejected *ali.VoiceEnrollmentError
		if errors.As(providerErr, &rejected) {
			definitive = rejected.Definitive
			if definitive {
				reason = "provider_rejected"
			}
		}
		updated, recordErr := model.RecordVoiceFailure(commitContext, info.UserId, profile.VoiceRef, reason, definitive)
		if recordErr != nil {
			service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist provider result")
			return
		}
		logger.LogWarn(c, "voice enrollment failed: "+providerErr.Error())
		writeVoiceResource(c, updated)
		return
	}
	profile, err = model.RecordVoiceProvider(commitContext, info.UserId, profile.VoiceRef, result.Output.VoiceID)
	if err != nil {
		logger.LogError(c, "voice provider ID persistence failed: "+err.Error())
		service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist provider identity")
		return
	}
	if err = service.SettleBilling(c, info, info.PriceData.Quota); err != nil {
		logger.LogError(c, "voice enrollment billing settlement failed: "+err.Error())
		service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_billing_failed", "voice enrollment billing settlement failed")
		return
	}
	other := model.NewLogOther()
	other.SetPublic("operation", "audio.voice_create")
	other.SetPublic("target_model", request.Model)
	model.RecordConsumeLog(c, info.UserId, model.RecordConsumeLogParams{ChannelId: info.ChannelId, ModelName: "voice-enrollment", TokenName: c.GetString("token_name"),
		Quota: info.PriceData.Quota, TokenId: info.TokenId, Group: info.UsingGroup, Content: "音色建档", Other: other})
	model.UpdateUserUsedQuotaAndRequestCount(info.UserId, info.PriceData.Quota)
	model.UpdateChannelUsedQuota(info.ChannelId, info.PriceData.Quota)
	if request.Kind == "design" {
		preview, previewErr := ali.DecodeVoicePreview(result)
		if previewErr == nil {
			previewErr = service.VoiceMedia.SavePreview(profile.VoiceRef, bytes.NewReader(preview))
		}
		if previewErr != nil {
			logger.LogWarn(c, "voice design preview unavailable: "+previewErr.Error())
		}
	}
	writeVoiceResource(c, profile)
}

func QueryVoice(c *gin.Context) {
	profile := c.MustGet("voice_profile").(*model.VoiceProfile)
	if profile.ProviderVoiceId == nil || profile.State == model.VoiceStateDeleted {
		writeVoiceResource(c, profile)
		return
	}
	info, supported := voiceRelayInfo(c, profile.TargetModel)
	if !supported {
		return
	}
	if int64(info.ChannelId) != profile.ChannelId {
		service.VoiceHTTPError(c, http.StatusForbidden, "voice_channel_mismatch", "voice is bound to a different channel")
		return
	}
	result, err := ali.CallVoiceEnrollment(c, info, ali.VoiceEnrollmentRequest{Model: "voice-enrollment", Input: ali.VoiceEnrollmentInput{Action: "query_voice", VoiceID: *profile.ProviderVoiceId, TargetModel: profile.TargetModel}})
	if err != nil {
		service.VoiceHTTPError(c, http.StatusBadGateway, "voice_provider_unavailable", "voice provider query failed")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	profile, err = model.RecordVoiceStatus(ctx, info.UserId, profile.VoiceRef, *profile.ProviderVoiceId, profile.TargetModel, result.Output.Status)
	if err != nil {
		service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist provider state")
		return
	}
	writeVoiceResource(c, profile)
}

func DeleteVoice(c *gin.Context) {
	profile := c.MustGet("voice_profile").(*model.VoiceProfile)
	userID := common.GetContextKeyInt(c, constant.ContextKeyUserId)
	var info *relaycommon.RelayInfo
	if profile.ProviderVoiceId != nil && profile.DeleteDispatchedTime == nil && profile.State != model.VoiceStateDeleted {
		var supported bool
		info, supported = voiceRelayInfo(c, profile.TargetModel)
		if !supported {
			return
		}
		if int64(info.ChannelId) != profile.ChannelId {
			service.VoiceHTTPError(c, http.StatusForbidden, "voice_channel_mismatch", "voice is bound to a different channel")
			return
		}
	}
	profile, dispatch, err := model.BeginVoiceDelete(c.Request.Context(), userID, profile.VoiceRef)
	if err != nil {
		service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist voice deletion")
		return
	}
	if dispatch {
		_, err = ali.CallVoiceEnrollment(c, info, ali.VoiceEnrollmentRequest{Model: "voice-enrollment", Input: ali.VoiceEnrollmentInput{Action: "delete_voice", VoiceID: *profile.ProviderVoiceId}})
		if err == nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
			defer cancel()
			profile, err = model.ConfirmVoiceDeletion(ctx, userID, profile.VoiceRef)
			if err != nil {
				service.VoiceHTTPError(c, http.StatusServiceUnavailable, "voice_storage_unavailable", "failed to persist supplier deletion ACK")
				return
			}
		} else {
			logger.LogWarn(c, "voice provider deletion remains pending: "+err.Error())
		}
	}
	if service.VoiceMedia != nil {
		if err = service.VoiceMedia.RemovePreview(profile.VoiceRef); err != nil {
			logger.LogWarn(c, "voice preview removal failed: "+err.Error())
		}
	}
	writeVoiceResource(c, profile)
}

func VoicePreview(c *gin.Context) {
	profile := c.MustGet("voice_profile").(*model.VoiceProfile)
	if service.VoiceMedia == nil || profile.State == model.VoiceStateDeleted || profile.DeleteRequestedTime != nil {
		service.VoiceHTTPError(c, http.StatusNotFound, "voice_preview_unavailable", "voice preview is unavailable")
		return
	}
	reader, err := service.VoiceMedia.OpenPreview(profile.VoiceRef)
	if err != nil {
		service.VoiceHTTPError(c, http.StatusNotFound, "voice_preview_unavailable", "voice preview is unavailable")
		return
	}
	defer reader.Close()
	c.Header("Content-Type", "audio/wav")
	c.Header("Cache-Control", "private, no-store")
	if _, err = io.Copy(c.Writer, reader); err != nil {
		logger.LogWarn(c, "voice preview write failed: "+err.Error())
	}
}

func VoiceMaterial(c *gin.Context) {
	if service.VoiceMedia == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	reader, err := service.VoiceMedia.OpenMaterial(c.Param("token"))
	if err != nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	defer func() {
		if err := reader.Close(); err != nil {
			logger.LogWarn(c, "voice material cleanup failed: "+err.Error())
		}
	}()
	head := make([]byte, 512)
	length, readErr := io.ReadFull(reader, head)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		logger.LogWarn(c, "voice material read failed: "+readErr.Error())
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", http.DetectContentType(head[:length]))
	c.Header("Cache-Control", "no-store")
	if _, err = c.Writer.Write(head[:length]); err != nil {
		logger.LogWarn(c, "voice material transfer failed: "+err.Error())
		return
	}
	if _, err = io.Copy(c.Writer, reader); err != nil {
		logger.LogWarn(c, "voice material transfer failed: "+err.Error())
	}
}
