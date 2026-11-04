// Package websearch 是 /v1/tools/search 的统一格式与提供方适配层（hasn fork 自有）。
//
// 节点只认一种请求与答复格式；每个搜索接口一个提供方，负责把统一请求翻成上游请求、
// 把上游答复归一为统一结果、把上游错误归一为统一状态码。提供方按渠道把路由名映射到的
// 上游模型名选取，不新增渠道类型。
package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
)

const (
	// UpstreamTimeout 是单次上游搜索的期限：换一次渠道仍在节点 20 秒总期限内。
	UpstreamTimeout = 8 * time.Second
	// MaxResponseBytes 是读取上游答复的上限，超出按答复无法识别处理。
	MaxResponseBytes = 1 << 20

	defaultCount   = 8
	maxCount       = 20
	maxQueryLength = 128
)

// SearchRequest 是统一请求：query 已去首尾空白，count 已校验在 1–20。
type SearchRequest struct {
	Query string
	Count int
}

// SearchResult 是统一结果的一条；date 为 RFC3339，解析不出时省略。
type SearchResult struct {
	Title    string `json:"title"`
	URL      string `json:"url"`
	Snippet  string `json:"snippet"`
	Summary  string `json:"summary"`
	SiteName string `json:"site_name"`
	Date     string `json:"date,omitempty"`
}

// SearchResponse 是统一答复；Results 恒为数组，零结果也写 []。
type SearchResponse struct {
	Results []SearchResult `json:"results"`
}

// SearchError 是归一后的失败：Status 只取 400、401、402、429、502。
type SearchError struct {
	Status  int
	Message string
}

func (e *SearchError) Error() string {
	return e.Message
}

func newSearchError(status int, format string, args ...any) *SearchError {
	return &SearchError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Provider 把统一请求翻成上游请求，并把上游答复归一。
type Provider interface {
	// BuildRequest 构造上游请求；baseURL 为渠道地址（可为空，用提供方默认地址）。
	// 查询串无法翻译时返回 400。
	BuildRequest(ctx context.Context, baseURL, apiKey string, req SearchRequest) (*http.Request, *SearchError)
	// ParseResponse 归一上游答复；ctx 只用于记录日志。
	ParseResponse(ctx context.Context, status int, body []byte, req SearchRequest) (SearchResponse, *SearchError)
}

// providers 以映射后的上游模型名登记提供方；以后加提供方只在这里登记。
var providers = map[string]Provider{
	"deepseek-search":    deepSeekProvider{},
	"tencent-wsa-search": tencentWSAProvider{now: time.Now},
	"tavily-search":      tavilyProvider{},
	"exa-search":         exaProvider{},
	"baidu-search":       baiduProvider{},
}

// ProviderFor 按渠道映射后的上游模型名取提供方；取不到即渠道配置错误，不回落默认提供方。
func ProviderFor(upstreamModel string) (Provider, bool) {
	provider, ok := providers[upstreamModel]
	return provider, ok
}

// ParseRequest 解析并校验统一请求体 {"model","query","count"}，其余字段忽略。
// count 先于其他字段校验：缺省为 8，不在 1–20 即拒绝。
func ParseRequest(body []byte) (SearchRequest, error) {
	var raw struct {
		Query *string `json:"query"`
		Count *int    `json:"count"`
	}
	if err := common.Unmarshal(body, &raw); err != nil {
		return SearchRequest{}, fmt.Errorf("invalid search request: %w", err)
	}
	count := defaultCount
	if raw.Count != nil {
		count = *raw.Count
		if count < 1 || count > maxCount {
			return SearchRequest{}, fmt.Errorf("count must be between 1 and %d", maxCount)
		}
	}
	if raw.Query == nil {
		return SearchRequest{}, errors.New("query is required")
	}
	query := strings.TrimSpace(*raw.Query)
	length := utf8.RuneCountInString(query)
	if length == 0 || length > maxQueryLength {
		return SearchRequest{}, fmt.Errorf("query must be 1 to %d characters", maxQueryLength)
	}
	return SearchRequest{Query: query, Count: count}, nil
}

// siteNameFromURL 是不给站点名的提供方的站点名：网址主机名去掉 www. 前缀，网址无法解析时为空。
func siteNameFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(parsed.Hostname(), "www.")
}

// joinURL 拼接渠道地址与接口路径，地址为空时用提供方默认地址。
func joinURL(baseURL, defaultBaseURL, path string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	return base + path
}
