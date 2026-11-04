package websearch

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"

	"github.com/tidwall/gjson"
)

const (
	exaDefaultBaseURL = "https://api.exa.ai"
	// exaMaxResults 是单次取回条数上限：Exa 每次超过 10 条的部分另行计费，截在 10 条让每次价格固定。
	exaMaxResults = 10
	// exaHighlightCharacters 是每条结果摘录的字符上限；摘录随 lite 搜索返回，不另计费。
	exaHighlightCharacters = 500
)

// exaProvider 对接 Exa Search：固定 lite 类型（$4/千次），带摘录作片段。
type exaProvider struct{}

// exaRequest 是 Exa Search 请求体；未用到的参数不发，取上游默认值。
type exaRequest struct {
	Query              string      `json:"query"`
	Type               string      `json:"type"`
	NumResults         int         `json:"numResults"`
	IncludeDomains     []string    `json:"includeDomains,omitempty"`
	ExcludeDomains     []string    `json:"excludeDomains,omitempty"`
	StartPublishedDate string      `json:"startPublishedDate,omitempty"`
	EndPublishedDate   string      `json:"endPublishedDate,omitempty"`
	Contents           exaContents `json:"contents"`
}

type exaContents struct {
	Highlights exaHighlights `json:"highlights"`
}

type exaHighlights struct {
	MaxCharacters int `json:"maxCharacters"`
}

// translateExaQuery 把统一查询串翻成 Exa 请求：全部 site: → includeDomains，全部 -site: → excludeDomains，
// after:/before: → 发布时间起止（北京时间当天 00:00:00 与 23:59:59）。余下文本为空时返回 400。
func translateExaQuery(req SearchRequest) (exaRequest, *SearchError) {
	operators, searchErr := parseSearchOperators(req.Query)
	if searchErr != nil {
		return exaRequest{}, searchErr
	}
	translated := exaRequest{
		Query:          operators.Text,
		Type:           "lite",
		NumResults:     min(req.Count, exaMaxResults),
		IncludeDomains: operators.Sites,
		ExcludeDomains: operators.Excludes,
		Contents:       exaContents{Highlights: exaHighlights{MaxCharacters: exaHighlightCharacters}},
	}
	if operators.After != "" {
		translated.StartPublishedDate = operators.After + "T00:00:00+08:00"
	}
	if operators.Before != "" {
		translated.EndPublishedDate = operators.Before + "T23:59:59+08:00"
	}
	return translated, nil
}

func (exaProvider) BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError) {
	translated, searchErr := translateExaQuery(req)
	if searchErr != nil {
		return nil, searchErr
	}
	body, err := common.Marshal(translated)
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "marshal search request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(baseURL, exaDefaultBaseURL, "/search"), bytes.NewReader(body))
	if err != nil {
		return nil, newSearchError(http.StatusBadGateway, "build search request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	return httpReq, nil
}

// ParseResponse 归一 Exa 答复：摘录拼成片段，站点名取网址主机名。402 是额度用尽或超出消费上限。
func (exaProvider) ParseResponse(_ context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError) {
	if status < 200 || status >= 300 {
		message := gjson.GetBytes(body, "error").String()
		if message == "" {
			message = http.StatusText(status)
		}
		switch status {
		case http.StatusBadRequest:
			return SearchResponse{}, newSearchError(http.StatusBadRequest, "exa search rejected the query: %s", message)
		case http.StatusUnauthorized, http.StatusForbidden:
			return SearchResponse{}, newSearchError(http.StatusUnauthorized, "exa search unauthorized: %s", message)
		case http.StatusPaymentRequired:
			return SearchResponse{}, newSearchError(http.StatusPaymentRequired, "exa search out of credits: %s", message)
		case http.StatusTooManyRequests:
			return SearchResponse{}, newSearchError(http.StatusTooManyRequests, "exa search rate limited: %s", message)
		default:
			return SearchResponse{}, newSearchError(http.StatusBadGateway, "exa search upstream status %d: %s", status, message)
		}
	}
	var answer struct {
		Results common.RawMessage `json:"results"`
	}
	if err := common.Unmarshal(body, &answer); err != nil || common.GetJsonType(answer.Results) != "array" {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "exa search answer has no results array")
	}
	var items []struct {
		Title         string   `json:"title"`
		URL           string   `json:"url"`
		PublishedDate string   `json:"publishedDate"`
		Highlights    []string `json:"highlights"`
	}
	if err := common.Unmarshal(answer.Results, &items); err != nil {
		return SearchResponse{}, newSearchError(http.StatusBadGateway, "exa search results unreadable: %v", err)
	}
	results := make([]SearchResult, 0, min(len(items), req.Count))
	for _, item := range items[:min(len(items), req.Count)] {
		snippet := strings.Join(item.Highlights, " … ")
		result := SearchResult{Title: item.Title, URL: item.URL, Snippet: snippet, Summary: snippet, SiteName: siteNameFromURL(item.URL)}
		if published, err := time.Parse(time.RFC3339, item.PublishedDate); err == nil {
			result.Date = published.Format(time.RFC3339)
		}
		results = append(results, result)
	}
	return SearchResponse{Results: results}, nil
}
