package controller

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var gatewayRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

// GetInternalAudioRequestSettlement 返回真实音频账务回执，不推断未派发，也不暴露内部计费字段。
// 查不到记录时返回协议规定的统一 unknown；存储故障则显式返回 503。
func GetInternalAudioRequestSettlement(c *gin.Context) {
	leaseID := c.Param("external_lease_id")
	requestID := c.Param("gateway_request_id")
	if !model.IsValidExternalLeaseId(leaseID) || !gatewayRequestIDPattern.MatchString(requestID) || c.Request.URL.RawQuery != "" {
		middleware.AbortWithInternalServiceError(c, http.StatusBadRequest, "invalid_request", "invalid audio receipt lookup", false)
		return
	}

	receipt, err := model.GetAudioSettlement(leaseID, requestID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.Header("Cache-Control", "no-store")
			c.JSON(http.StatusOK, dto.AudioRequestSettlementResult{
				ExternalLeaseId:  leaseID,
				GatewayRequestId: requestID,
				BillingStatus:    "unknown",
				DispatchState:    "unknown",
			})
			return
		}
		middleware.AbortWithInternalServiceError(c, http.StatusServiceUnavailable, "store_unavailable", "audio receipt storage is unavailable", true)
		return
	}

	result := dto.AudioRequestSettlementResult{
		ExternalLeaseId:  receipt.ExternalLeaseId,
		GatewayRequestId: receipt.GatewayRequestId,
		BillingStatus:    receipt.BillingStatus,
		DispatchState:    receipt.DispatchState,
	}
	settlementID := receipt.AudioRequestSettlementId
	revision := receipt.Revision
	result.AudioRequestSettlementId = &settlementID
	result.Revision = &revision
	if receipt.CompletedTime != nil {
		completed := receipt.CompletedTime.UTC().Format("2006-01-02T15:04:05.000Z")
		result.CompletedTime = &completed
	}
	if strings.TrimSpace(result.BillingStatus) == "" || strings.TrimSpace(result.DispatchState) == "" {
		middleware.AbortWithInternalServiceError(c, http.StatusServiceUnavailable, "store_unavailable", "audio receipt storage is incomplete", true)
		return
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, result)
}
