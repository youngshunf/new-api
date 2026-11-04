package websearch

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	baiduDefaultBaseURL = "https://qianfan.baidubce.com"
	baiduDateLayout     = "2006-01-02 15:04:05"
	// baiduTitleFromSnippetRunes 是标题不可用时从片段截取的字符数。
	baiduTitleFromSnippetRunes = 40
	// baiduEarliestDate 是只给 before: 时补的起点：起点为 1970-01-01 时百度实测忽略整个时间范围。
	baiduEarliestDate = "2000-01-01"
)

// baiduErrorStatuses 把百度千帆答复体里的 code 按关键词（不分大小写）归一为统一状态，先匹配先得；
// 未列出的一律 502。千帆的业务错误可能是 HTTP 200 加 code。
var baiduErrorStatuses = []struct {
	keyword string
	status  int
}{
	{"invalidargument", http.StatusBadRequest},
	{"invalidparam", http.StatusBadRequest},
	{"auth", http.StatusUnauthorized},
	{"permission", http.StatusUnauthorized},
	{"overdue", http.StatusPaymentRequired},
	{"arrear", http.StatusPaymentRequired},
	{"quota", http.StatusPaymentRequired},
	{"ratelimit", http.StatusTooManyRequests},
	{"rate_limit", http.StatusTooManyRequests},
	{"qps", http.StatusTooManyRequests},
	{"rpm", http.StatusTooManyRequests},
}

// baiduProvider 对接百度千帆「百度搜索」（/v2/ai_search/web_search，API Key 方式）。
type baiduProvider struct {
	now func() time.Time
}

// baiduRequest 是百度搜索请求体；未用到的参数不发，取上游默认值。
type baiduRequest struct {
	Messages           []baiduMessage            `json:"messages"`
	ResourceTypeFilter []baiduResourceTypeFilter `json:"resource_type_filter"`
	SearchFilter       *baiduSearchFilter        `json:"search_filter,omitempty"`
	BlockWebsites      []string                  `json:"block_websites,omitempty"`
}

type baiduMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type baiduResourceTypeFilter struct {
	Type string `json:"type"`
	TopK int    `json:"top_k"`
}

type baiduSearchFilter struct {
	Match *baiduSiteMatch `json:"match,omitempty"`
	Range *baiduTimeRange `json:"range,omitempty"`
}

type baiduSiteMatch struct {
	Site []string `json:"site"`
}

type baiduTimeRange struct {
	PageTime baiduPageTime `json:"page_time"`
}

type baiduPageTime struct {
	Gte string `json:"gte"`
	Lte string `json:"lte"`
}

// translateBaiduQuery 把统一查询串翻成百度搜索请求：全部 site: → search_filter.match.site，
// 全部 -site: → block_websites，after:/before: → search_filter.range.page_time 的 gte/lte。余下文本为空时返回 400。
// 时间范围两端总是都发（缺起点补 2000-01-01、缺终点补北京时间当天）：百度实测只给 gte 时，
// 与站点限定同用即被忽略；只给 lte 时一律被忽略。
func translateBaiduQuery(req SearchRequest, now time.Time) (baiduRequest, *SearchError) {
	operators, searchErr := parseSearchOperators(req.Query)
	if searchErr != nil {
		return baiduRequest{}, searchErr
	}
	translated := baiduRequest{
		Messages:           []baiduMessage{{Role: "user", Content: operators.Text}},
		ResourceTypeFilter: []baiduResourceTypeFilter{{Type: "web", TopK: req.Count}},
		BlockWebsites:      operators.Excludes,
	}
	var filter baiduSearchFilter
	if len(operators.Sites) > 0 {
		filter.Match = &baiduSiteMatch{Site: operators.Sites}
	}
	if operators.After != "" || operators.Before != "" {
		pageTime := baiduPageTime{Gte: operators.After, Lte: operators.Before}
		if pageTime.Gte == "" {
			pageTime.Gte = baiduEarliestDate
		}
		if pageTime.Lte == "" {
			pageTime.Lte = now.In(beijingTime).Format(searchOperatorDateLayout)
		}
		filter.Range = &baiduTimeRange{PageTime: pageTime}
	}
	if filter.Match != nil || filter.Range != nil {
		translated.SearchFilter = &filter
	}
	return translated, nil
}

func (p baiduProvider) BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError) {
	translated, searchErr := translateBaiduQuery(req, p.now())
	if searchErr != nil {
		return nil, searchErr
	}
	body, err := common.Marshal(translated)
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "marshal search request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, baiduDefaultBaseURL, "/v2/ai_search/web_search"), bytes.NewReader(body))
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "build search request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return httpReq, nil
}

// ParseResponse 归一百度搜索答复。错误可能是非 2xx，也可能是 HTTP 200 加 code；
// 日期无时区，按北京时间理解；标题是残缺 JSON 文本时（实测微博、公众号条目会这样）改用片段开头。
func (baiduProvider) ParseResponse(_ context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError) {
	var answer struct {
		RequestID  string            `json:"request_id"`
		Code       common.RawMessage `json:"code"`
		Message    string            `json:"message"`
		References common.RawMessage `json:"references"`
	}
	decodeErr := common.Unmarshal(body, &answer)
	code := strings.Trim(string(answer.Code), `"`)
	if code == "0" || code == "null" {
		code = ""
	}
	if status < 200 || status >= 300 || code != "" {
		return SearchResponse{}, baiduError(status, code, answer.Message, answer.RequestID)
	}
	if decodeErr != nil || common.GetJsonType(answer.References) != "array" {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "baidu search answer has no references array")
	}
	var items []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Snippet string `json:"snippet"`
		Content string `json:"content"`
		Website string `json:"website"`
		Date    string `json:"date"`
	}
	if err := common.Unmarshal(answer.References, &items); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "baidu search references unreadable: %v", err)
	}
	results := make([]SearchResult, 0, min(len(items), req.Count))
	for _, item := range items[:min(len(items), req.Count)] {
		snippet := strings.TrimSpace(item.Snippet)
		summary := strings.TrimSpace(item.Content)
		if summary == "" {
			summary = snippet
		}
		title := strings.TrimSpace(item.Title)
		if strings.HasPrefix(title, "{") {
			title = snippet
			if runes := []rune(snippet); len(runes) > baiduTitleFromSnippetRunes {
				title = string(runes[:baiduTitleFromSnippetRunes]) + "…"
			}
		}
		result := SearchResult{Title: title, URL: item.URL, Snippet: snippet, Summary: summary, SiteName: item.Website}
		if date, err := time.ParseInLocation(baiduDateLayout, item.Date, beijingTime); err == nil {
			result.Date = date.Format(time.RFC3339)
		}
		results = append(results, result)
	}
	return SearchResponse{Results: results}, nil
}

// baiduError 按 HTTP 状态与答复 code 归一错误：先看明确的 HTTP 状态，再按 code 关键词。
func baiduError(status int, code, message, requestID string) *SearchError {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return newSearchError(http.StatusUnauthorized, "baidu search unauthorized %s: %s (request %s)", code, message, requestID)
	case http.StatusPaymentRequired:
		return newSearchError(http.StatusPaymentRequired, "baidu search payment required %s: %s (request %s)", code, message, requestID)
	case http.StatusTooManyRequests:
		return newSearchError(http.StatusTooManyRequests, "baidu search rate limited %s: %s (request %s)", code, message, requestID)
	}
	lowered := strings.ToLower(code + " " + message)
	for _, entry := range baiduErrorStatuses {
		if strings.Contains(lowered, entry.keyword) {
			return newSearchError(entry.status, "baidu search error %s: %s (request %s)", code, message, requestID)
		}
	}
	if status == http.StatusBadRequest {
		return newSearchError(http.StatusBadRequest, "baidu search rejected the query %s: %s (request %s)", code, message, requestID)
	}
	return newSearchError(http.StatusBadGateway, "baidu search error status %d %s: %s (request %s)", status, code, message, requestID)
}
