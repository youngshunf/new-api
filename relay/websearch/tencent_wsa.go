package websearch

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
)

const (
	tencentWSADefaultBaseURL = "https://api.wsa.cloud.tencent.com"
	tencentWSAMaxExcludes    = 5
	tencentWSADateLayout     = "2006/01/02 15:04:05"
	freshnessDateLayout      = "2006-01-02"
	freshnessEarliestDate    = "1970-01-01"
)

// beijingTime 是元宝答复日期与 Freshness「当天」的时区；用固定偏移，不依赖 tzdata。
var beijingTime = time.FixedZone("UTC+8", 8*60*60)

// tencentWSAErrorStatuses 按前缀把 Response.Error.Code 归一为统一状态，先匹配先得；
// 未列出的错误码一律 502。
var tencentWSAErrorStatuses = []struct {
	prefix string
	status int
}{
	{"InvalidParameter", http.StatusBadRequest},
	{"UnauthorizedOperation", http.StatusUnauthorized},
	{"AuthFailure", http.StatusUnauthorized},
	{"RequestLimitExceeded", http.StatusTooManyRequests},
	{"ResourceNotFound", http.StatusPaymentRequired},
	{"ResourceUnavailable", http.StatusPaymentRequired},
	{"ResourceInsufficient", http.StatusPaymentRequired},
	{"LimitExceeded", http.StatusPaymentRequired},
}

// tencentWSAProvider 对接腾讯元宝搜索 SearchPro（API KEY 方式，极速版）。
// 极速版不支持 Cnt，固定返回 10 条，由本适配器截到 count 条。
type tencentWSAProvider struct {
	now func() time.Time
}

// tencentWSAQuery 是从统一查询串翻译出的 SearchPro 参数。
type tencentWSAQuery struct {
	Query     string `json:"Query"`
	Site      string `json:"Site,omitempty"`
	Freshness string `json:"Freshness,omitempty"`
}

// translateTencentWSAQuery 从查询串取出运算符并翻成 SearchPro 参数：
// 第一个 site: → Site；没有 site: 时全部 -site: 合成 exclude:a|b（最多 5 个）；
// after:/before: 合成 Freshness=起,止（缺一端用 1970-01-01 或北京时间当天）；lang: 删去。
// 引号短语内的词不当运算符；日期不是 YYYY-MM-DD 的 after:/before: 当普通文本。余下文本为空时返回 400。
func translateTencentWSAQuery(query string, now time.Time) (tencentWSAQuery, *SearchError) {
	var text []string
	var site, after, before string
	var excludes []string
	inQuote := false
	for _, token := range strings.Fields(query) {
		quoted := inQuote
		if strings.Count(token, `"`)%2 == 1 {
			inQuote = !inQuote
		}
		if quoted {
			text = append(text, token)
			continue
		}
		if value, ok := strings.CutPrefix(token, "-site:"); ok && value != "" {
			excludes = append(excludes, value)
			continue
		}
		if value, ok := strings.CutPrefix(token, "site:"); ok && value != "" {
			if site == "" {
				site = value
			}
			continue
		}
		if value, ok := strings.CutPrefix(token, "after:"); ok && value != "" {
			// 日期不是 YYYY-MM-DD 时原样留在查询词里，不回 400：400 不换渠道，别的提供方可能认得这种写法。
			if _, err := time.Parse(freshnessDateLayout, value); err != nil {
				text = append(text, token)
				continue
			}
			if after == "" {
				after = value
			}
			continue
		}
		if value, ok := strings.CutPrefix(token, "before:"); ok && value != "" {
			// 日期不是 YYYY-MM-DD 时原样留在查询词里，不回 400：400 不换渠道，别的提供方可能认得这种写法。
			if _, err := time.Parse(freshnessDateLayout, value); err != nil {
				text = append(text, token)
				continue
			}
			if before == "" {
				before = value
			}
			continue
		}
		if value, ok := strings.CutPrefix(token, "lang:"); ok && value != "" {
			continue
		}
		text = append(text, token)
	}
	translated := tencentWSAQuery{Query: strings.Join(text, " "), Site: site}
	if translated.Query == "" {
		return tencentWSAQuery{}, newSearchError(http.StatusBadRequest, "query has no search text besides operators")
	}
	if site == "" && len(excludes) > 0 {
		translated.Site = "exclude:" + strings.Join(excludes[:min(len(excludes), tencentWSAMaxExcludes)], "|")
	}
	if after != "" || before != "" {
		if after == "" {
			after = freshnessEarliestDate
		}
		if before == "" {
			before = now.In(beijingTime).Format(freshnessDateLayout)
		}
		translated.Freshness = after + "," + before
	}
	return translated, nil
}

func (p tencentWSAProvider) BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError) {
	translated, searchErr := translateTencentWSAQuery(req.Query, p.now())
	if searchErr != nil {
		return nil, searchErr
	}
	body, err := common.Marshal(translated)
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "marshal search request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, tencentWSADefaultBaseURL, "/SearchPro"), bytes.NewReader(body))
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "build search request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return httpReq, nil
}

// ParseResponse 归一 SearchPro 答复。元宝的业务错误也是 HTTP 200，错误在 Response.Error.Code，
// 不能按状态码判成功；Pages 每项是一段 JSON 字符串，坏项只丢该条并记 warn。
func (tencentWSAProvider) ParseResponse(ctx context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError) {
	if status == http.StatusUnauthorized {
		return SearchResponse{}, newSearchError(http.StatusUnauthorized, "tencent wsa search unauthorized")
	}
	if status != http.StatusOK {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tencent wsa search upstream status %d", status)
	}
	var answer struct {
		Response struct {
			Error *struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
			RequestID string            `json:"RequestId"`
			Pages     common.RawMessage `json:"Pages"`
		} `json:"Response"`
	}
	if err := common.Unmarshal(body, &answer); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tencent wsa search answer unreadable: %v", err)
	}
	if upstreamErr := answer.Response.Error; upstreamErr != nil && upstreamErr.Code != "" {
		status := http.StatusBadGateway
		for _, entry := range tencentWSAErrorStatuses {
			if strings.HasPrefix(upstreamErr.Code, entry.prefix) {
				status = entry.status
				break
			}
		}
		return SearchResponse{}, newSearchError(status, "tencent wsa search error %s: %s (request %s)", upstreamErr.Code, upstreamErr.Message, answer.Response.RequestID)
	}
	if common.GetJsonType(answer.Response.Pages) != "array" {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tencent wsa search answer has neither Error nor Pages")
	}
	var pages []common.RawMessage
	if err := common.Unmarshal(answer.Response.Pages, &pages); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tencent wsa search Pages unreadable: %v", err)
	}
	results := make([]SearchResult, 0, min(len(pages), req.Count))
	for i, raw := range pages {
		if len(results) == req.Count {
			break
		}
		var encoded string
		var page struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Date    string `json:"date"`
			Passage string `json:"passage"`
			Content string `json:"content"`
			Site    string `json:"site"`
		}
		if err := common.Unmarshal(raw, &encoded); err != nil {
			logger.LogWarn(ctx, common.LogText("tencent wsa search page %d is not a JSON string, dropped (request %s): %s", i, answer.Response.RequestID, err.Error()))
			continue
		}
		if err := common.UnmarshalJsonStr(encoded, &page); err != nil {
			logger.LogWarn(ctx, common.LogText("tencent wsa search page %d is not valid JSON, dropped (request %s): %s", i, answer.Response.RequestID, err.Error()))
			continue
		}
		result := SearchResult{
			Title:    page.Title,
			URL:      page.URL,
			Snippet:  page.Passage,
			Summary:  page.Content,
			SiteName: page.Site,
		}
		if result.Summary == "" {
			result.Summary = page.Passage
		}
		if date, err := time.ParseInLocation(tencentWSADateLayout, page.Date, beijingTime); err == nil {
			result.Date = date.Format(time.RFC3339)
		}
		results = append(results, result)
	}
	return SearchResponse{Results: results}, nil
}
