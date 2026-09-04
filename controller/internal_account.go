package controller

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const maxInternalAccountRequestBody = 1024

type internalAccountProvisionRequest struct {
	ProvisionKey string `json:"provision_key"`
}

var internalAccountProvisionAllowedFields = map[string]struct{}{
	"provision_key": {},
}

func decodeInternalAccountProvisionRequest(c *gin.Context) (*internalAccountProvisionRequest, error) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxInternalAccountRequestBody+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	if len(body) > maxInternalAccountRequestBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxInternalAccountRequestBody)
	}
	var probe map[string]any
	if err := common.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	for key := range probe {
		if _, ok := internalAccountProvisionAllowedFields[key]; !ok {
			return nil, fmt.Errorf("unknown field: %s", key)
		}
	}
	var req internalAccountProvisionRequest
	if err := common.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("request body does not match the account provision schema")
	}
	return &req, nil
}

func abortWithInternalAccountError(c *gin.Context, err *model.InternalAccountError) {
	if err.Retryable && err.Cause != "" {
		logger.LogWarn(c, fmt.Sprintf("internal account request failed code=%s cause=%s", err.Code, err.Cause))
	}
	middleware.AbortWithInternalServiceError(c, err.HTTPStatus, err.Code, err.Message, err.Retryable)
}

func PostInternalAccount(c *gin.Context) {
	req, decodeErr := decodeInternalAccountProvisionRequest(c)
	if decodeErr != nil {
		middleware.AbortWithInternalServiceError(c, http.StatusBadRequest, model.InternalAccountErrorInvalidRequest, decodeErr.Error(), false)
		return
	}
	userId, accountErr := model.ProvisionInternalAccount(req.ProvisionKey)
	if accountErr != nil {
		abortWithInternalAccountError(c, accountErr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"newapi_user_id": userId})
}

func GetInternalAccount(c *gin.Context) {
	userId, err := strconv.Atoi(strings.TrimSpace(c.Param("newapi_user_id")))
	if err != nil || userId <= 0 {
		middleware.AbortWithInternalServiceError(c, http.StatusBadRequest, model.InternalAccountErrorInvalidRequest,
			"newapi_user_id must be a positive integer", false)
		return
	}
	status, accountErr := model.GetInternalAccountStatus(userId)
	if accountErr != nil {
		abortWithInternalAccountError(c, accountErr)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": status})
}
