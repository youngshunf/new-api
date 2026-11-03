package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/pkg/retrycontrol"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relay/websearch"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// WebSearchHelper 是 /v1/tools/search 的统一处理器：按渠道映射后的上游模型名取提供方，
// 解析并校验统一请求，按渠道代理发出单次限时的上游请求，把答复归一为统一格式写回，
// 统一答复有结果才按路由名的单次价格结算，否则退回预扣。
//
// 失败按统一状态返回：400 不换渠道；401、402、429、502 与提供方未登记都可换下一个渠道。
// 上游超时也归 502，因为全局规则对 504 一律不换渠道。
func WebSearchHelper(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	info.InitChannelMeta(c)

	request, ok := info.Request.(*dto.DeepSeekToolsRequest)
	if !ok {
		return types.NewErrorWithStatusCode(
			fmt.Errorf("invalid request type, expected *dto.DeepSeekToolsRequest, got %T", info.Request),
			types.ErrorCodeInvalidRequest,
			http.StatusBadRequest,
			types.ErrOptionWithSkipRetry(),
		)
	}
	if err := helper.ModelMappedHelper(c, info, request); err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	provider, ok := websearch.ProviderFor(info.UpstreamModelName)
	if !ok {
		// 渠道配置错误：该渠道本次失败、换下一个渠道，不回落默认提供方。
		// 不用 channel: 错误码，免得开了自动禁用时把渠道禁掉。
		logger.LogError(c, common.LogText("web search channel #%d maps %s to %s, which is not a registered search provider", info.ChannelId, info.OriginModelName, info.UpstreamModelName))
		return types.NewErrorWithStatusCode(
			fmt.Errorf("channel maps %s to an unknown search provider", info.OriginModelName),
			types.ErrorCodeModelNotFound,
			http.StatusInternalServerError,
		)
	}

	searchRequest, err := websearch.ParseRequest(request.RawBody)
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), websearch.UpstreamTimeout)
	defer cancel()
	upstreamRequest, searchErr := provider.BuildRequest(ctx, info.ChannelBaseUrl, info.ApiKey, searchRequest)
	if searchErr != nil {
		return webSearchError(searchErr)
	}

	client, err := service.GetHttpClientWithProxySettings(info.ChannelSetting.Proxy, info.ChannelSetting)
	if err != nil {
		return types.NewErrorWithStatusCode(fmt.Errorf("new proxy http client failed: %w", err), types.ErrorCodeDoRequestFailed, http.StatusBadGateway)
	}
	// 与 relay/channel 的发送路径一致：插桩记录上游是否收到过字节，供 daemon 判断能否重放。
	dispatchTracker := retrycontrol.Tracker(c)
	dispatchTracker.MarkInstrumentedAttempt()
	upstreamRequest = retrycontrol.TraceRequest(upstreamRequest, dispatchTracker)
	common.SetContextKey(c, constant.ContextKeyUpstreamRequestOrigin, upstreamRequest.URL.Scheme+"://"+upstreamRequest.URL.Host)

	resp, err := client.Do(upstreamRequest)
	if err != nil {
		logger.LogWarn(c, common.LogText("web search request failed: %s", err.Error()))
		return types.NewErrorWithStatusCode(err, types.ErrorCodeDoRequestFailed, http.StatusBadGateway, types.ErrOptionWithHideErrMsg("upstream error: do request failed"))
	}
	defer resp.Body.Close()
	dispatchTracker.MarkResponseReceived()

	body, err := io.ReadAll(io.LimitReader(resp.Body, websearch.MaxResponseBytes+1))
	if err != nil {
		return types.NewErrorWithStatusCode(fmt.Errorf("read search response: %w", err), types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway)
	}
	if len(body) > websearch.MaxResponseBytes {
		return types.NewErrorWithStatusCode(errors.New("search response exceeds 1 MiB"), types.ErrorCodeBadResponse, http.StatusBadGateway)
	}
	searchResponse, searchErr := provider.ParseResponse(c, resp.StatusCode, body, searchRequest)
	if searchErr != nil {
		return webSearchError(searchErr)
	}

	responseBody, err := common.Marshal(searchResponse)
	if err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodeJsonMarshalFailed, http.StatusInternalServerError, types.ErrOptionWithSkipRetry())
	}
	c.Data(http.StatusOK, "application/json", responseBody)

	// 统一答复有结果才结算一次（不看 count、不引入计费乘数），零结果退回预扣。
	if len(searchResponse.Results) == 0 {
		if info.Billing != nil {
			info.Billing.Refund(c)
		}
		return nil
	}
	service.PostTextConsumeQuota(c, info, &dto.Usage{}, nil)
	return nil
}

// webSearchError 把提供方归一的失败转成中继错误：400 是查询本身被拒，换渠道多半同样被拒，不重试。
func webSearchError(searchErr *websearch.SearchError) *types.NewAPIError {
	if searchErr.Status == http.StatusBadRequest {
		return types.NewErrorWithStatusCode(searchErr, types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	return types.NewErrorWithStatusCode(searchErr, types.ErrorCodeBadResponseStatusCode, searchErr.Status)
}
