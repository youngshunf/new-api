package websearch

import (
	"bytes"
	"context"
	"net/http"

	"github.com/QuantumNous/new-api/common"

	"github.com/tidwall/gjson"
)

const deepSeekDefaultBaseURL = "https://api.deepseek.com"

// deepSeekProvider 对接 DeepSeek Search：请求体与答复字段本就是统一格式的超集，原样取子集。
type deepSeekProvider struct{}

func (deepSeekProvider) BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError) {
	body, err := common.Marshal(map[string]any{"query": req.Query, "count": req.Count})
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "marshal search request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, deepSeekDefaultBaseURL, "/v1/tools/search"), bytes.NewReader(body))
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "build search request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	return httpReq, nil
}

func (deepSeekProvider) ParseResponse(_ context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError) {
	if status < 200 || status >= 300 {
		message := gjson.GetBytes(body, "error.message").String()
		if message == "" {
			message = http.StatusText(status)
		}
		switch status {
		case http.StatusBadRequest:
			return SearchResponse{}, newSearchError(http.StatusBadRequest, "deepseek search rejected the query: %s", message)
		case http.StatusUnauthorized, http.StatusForbidden:
			return SearchResponse{}, newSearchError(http.StatusUnauthorized, "deepseek search unauthorized: %s", message)
		case http.StatusPaymentRequired:
			return SearchResponse{}, newSearchError(http.StatusPaymentRequired, "deepseek search payment required: %s", message)
		case http.StatusTooManyRequests:
			return SearchResponse{}, newSearchError(http.StatusTooManyRequests, "deepseek search rate limited: %s", message)
		default:
			return SearchResponse{}, newSearchError(http.StatusBadGateway, "deepseek search upstream status %d: %s", status, message)
		}
	}
	var answer struct {
		Results common.RawMessage `json:"results"`
	}
	if err := common.Unmarshal(body, &answer); err != nil || common.GetJsonType(answer.Results) != "array" {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "deepseek search answer has no results array")
	}
	// date 只在是字符串时取用，其余形状省略，不让一个字段拖垮整次答复。
	var items []struct {
		Title    string            `json:"title"`
		URL      string            `json:"url"`
		Snippet  string            `json:"snippet"`
		Summary  string            `json:"summary"`
		SiteName string            `json:"site_name"`
		Date     common.RawMessage `json:"date"`
	}
	if err := common.Unmarshal(answer.Results, &items); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "deepseek search results unreadable: %v", err)
	}
	results := make([]SearchResult, 0, min(len(items), req.Count))
	for _, item := range items[:min(len(items), req.Count)] {
		result := SearchResult{Title: item.Title, URL: item.URL, Snippet: item.Snippet, Summary: item.Summary, SiteName: item.SiteName}
		if common.GetJsonType(item.Date) == "string" {
			_ = common.Unmarshal(item.Date, &result.Date)
		}
		results = append(results, result)
	}
	return SearchResponse{Results: results}, nil
}
