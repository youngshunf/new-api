package websearch

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/tidwall/gjson"
)

const (
	tavilyDefaultBaseURL = "https://api.tavily.com"
	// tavilyStatusPlanLimit 是套餐额度（免费档每月 1000 点）用尽，tavilyStatusPayGoLimit 是按量付费上限用尽。
	tavilyStatusPlanLimit  = 432
	tavilyStatusPayGoLimit = 433
)

// tavilyProvider 对接 Tavily Search。固定 basic 深度（每次 1 点），并要求带发布日期；
// 站点与日期运算符翻成 Tavily 自己的参数，不留在查询词里。
type tavilyProvider struct{}

// tavilyRequest 是 Tavily Search 请求体；未用到的参数不发，取上游默认值。
type tavilyRequest struct {
	Query                string   `json:"query"`
	MaxResults           int      `json:"max_results"`
	SearchDepth          string   `json:"search_depth"`
	IncludePublishedDate bool     `json:"include_published_date"`
	IncludeDomains       []string `json:"include_domains,omitempty"`
	ExcludeDomains       []string `json:"exclude_domains,omitempty"`
	StartDate            string   `json:"start_date,omitempty"`
	EndDate              string   `json:"end_date,omitempty"`
}

// translateTavilyQuery 把统一查询串翻成 Tavily 请求：全部 site: → include_domains，
// 全部 -site: → exclude_domains，after:/before: → start_date/end_date。余下文本为空时返回 400。
func translateTavilyQuery(req SearchRequest) (tavilyRequest, *SearchError) {
	operators, searchErr := parseSearchOperators(req.Query)
	if searchErr != nil {
		return tavilyRequest{}, searchErr
	}
	return tavilyRequest{
		Query:                operators.Text,
		MaxResults:           req.Count,
		SearchDepth:          "basic",
		IncludePublishedDate: true,
		IncludeDomains:       operators.Sites,
		ExcludeDomains:       operators.Excludes,
		StartDate:            operators.After,
		EndDate:              operators.Before,
	}, nil
}

func (tavilyProvider) BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError) {
	translated, searchErr := translateTavilyQuery(req)
	if searchErr != nil {
		return nil, searchErr
	}
	body, err := common.Marshal(translated)
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "marshal search request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, tavilyDefaultBaseURL, "/search"), bytes.NewReader(body))
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "build search request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return httpReq, nil
}

// ParseResponse 归一 Tavily 答复。432/433 是额度用尽，按 402 换渠道；
// Tavily 不给站点名，取网址主机名（去 www.）。
func (tavilyProvider) ParseResponse(_ context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError) {
	if status < 200 || status >= 300 {
		message := gjson.GetBytes(body, "detail.error").String()
		if message == "" {
			message = http.StatusText(status)
		}
		switch status {
		case http.StatusBadRequest:
			return SearchResponse{}, newSearchError(http.StatusBadRequest, "tavily search rejected the query: %s", message)
		case http.StatusUnauthorized, http.StatusForbidden:
			return SearchResponse{}, newSearchError(http.StatusUnauthorized, "tavily search unauthorized: %s", message)
		case tavilyStatusPlanLimit, tavilyStatusPayGoLimit:
			return SearchResponse{}, newSearchError(http.StatusPaymentRequired, "tavily search usage limit exceeded (%d): %s", status, message)
		case http.StatusTooManyRequests:
			return SearchResponse{}, newSearchError(http.StatusTooManyRequests, "tavily search rate limited: %s", message)
		default:
			return SearchResponse{}, newSearchError(http.StatusBadGateway, "tavily search upstream status %d: %s", status, message)
		}
	}
	var answer struct {
		Results common.RawMessage `json:"results"`
	}
	if err := common.Unmarshal(body, &answer); err != nil || common.GetJsonType(answer.Results) != "array" {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tavily search answer has no results array")
	}
	// published_date 只在是字符串时取用，其余形状省略。
	var items []struct {
		Title         string            `json:"title"`
		URL           string            `json:"url"`
		Content       string            `json:"content"`
		PublishedDate common.RawMessage `json:"published_date"`
	}
	if err := common.Unmarshal(answer.Results, &items); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "tavily search results unreadable: %v", err)
	}
	results := make([]SearchResult, 0, min(len(items), req.Count))
	for _, item := range items[:min(len(items), req.Count)] {
		result := SearchResult{Title: item.Title, URL: item.URL, Snippet: item.Content, Summary: item.Content, SiteName: siteNameFromURL(item.URL)}
		var published string
		if common.GetJsonType(item.PublishedDate) == "string" && common.Unmarshal(item.PublishedDate, &published) == nil {
			result.Date = tavilyDate(published)
		}
		results = append(results, result)
	}
	return SearchResponse{Results: results}, nil
}

// tavilyDate 把 Tavily 的发布日期（实测为 RFC1123 GMT 写法）转成 RFC3339；认不出时省略。
func tavilyDate(value string) string {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339, searchOperatorDateLayout} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format(time.RFC3339)
		}
	}
	return ""
}
