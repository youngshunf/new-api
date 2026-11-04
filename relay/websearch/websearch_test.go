package websearch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 统一请求：count 缺省 8、越界与非整数拒绝；query 去首尾空白后按 Unicode 字符计 1–128。
func TestParseRequest(t *testing.T) {
	long := strings.Repeat("搜", 128)
	tests := []struct {
		name, body string
		want       SearchRequest
		wantErr    bool
	}{
		{name: "count defaults to 8", body: `{"model":"web-search","query":"  deepseek  "}`, want: SearchRequest{Query: "deepseek", Count: 8}},
		{name: "explicit count", body: `{"query":"deepseek","count":20}`, want: SearchRequest{Query: "deepseek", Count: 20}},
		{name: "128 characters", body: `{"query":"` + long + `","count":1}`, want: SearchRequest{Query: long, Count: 1}},
		{name: "129 characters", body: `{"query":"` + long + `x"}`, wantErr: true},
		{name: "count 0", body: `{"query":"deepseek","count":0}`, wantErr: true},
		{name: "count 21", body: `{"query":"deepseek","count":21}`, wantErr: true},
		{name: "fractional count", body: `{"query":"deepseek","count":2.5}`, wantErr: true},
		{name: "huge count", body: `{"query":"deepseek","count":18446744073686646784}`, wantErr: true},
		{name: "blank query", body: `{"query":"   "}`, wantErr: true},
		{name: "missing query", body: `{"count":3}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRequest([]byte(tc.body))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestProviderForRegisteredUpstreamModels(t *testing.T) {
	for _, name := range []string{"deepseek-search", "tencent-wsa-search", "tavily-search"} {
		_, ok := ProviderFor(name)
		assert.True(t, ok, name)
	}
	_, ok := ProviderFor("web-search")
	assert.False(t, ok, "the routing name itself is not a provider")
}

// 运算符翻译：北京时间已到 11-04 而 UTC 仍是 11-03，「当天」按北京时间取。
func TestTranslateTencentWSAQuery(t *testing.T) {
	now := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name, query string
		want        tencentWSAQuery
		wantStatus  int
	}{
		{name: "plain text", query: "南极  冰下 湖泊", want: tencentWSAQuery{Query: "南极 冰下 湖泊"}},
		{name: "first site wins and drops exclusions", query: "site:a.com 南极 site:b.com -site:c.com",
			want: tencentWSAQuery{Query: "南极", Site: "a.com"}},
		{name: "exclusions capped at five", query: "南极 -site:a -site:b -site:c -site:d -site:e -site:f",
			want: tencentWSAQuery{Query: "南极", Site: "exclude:a|b|c|d|e"}},
		{name: "after only ends today in Beijing", query: "after:2026-10-01 南极",
			want: tencentWSAQuery{Query: "南极", Freshness: "2026-10-01,2026-11-04"}},
		{name: "before only starts at the epoch", query: "南极 before:2026-10-31",
			want: tencentWSAQuery{Query: "南极", Freshness: "1970-01-01,2026-10-31"}},
		{name: "both ends", query: "after:2026-10-01 before:2026-10-31 南极",
			want: tencentWSAQuery{Query: "南极", Freshness: "2026-10-01,2026-10-31"}},
		{name: "lang dropped and quoted phrase kept", query: `lang:zh "site:x 冰下" 湖泊`,
			want: tencentWSAQuery{Query: `"site:x 冰下" 湖泊`}},
		{name: "operator without value is text", query: "site: 南极", want: tencentWSAQuery{Query: "site: 南极"}},
		{name: "only operators", query: "site:a.com lang:en", wantStatus: http.StatusBadRequest},
		{name: "malformed date stays as text", query: "after:2026/10/01 南极",
			want: tencentWSAQuery{Query: "after:2026/10/01 南极"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, searchErr := translateTencentWSAQuery(tc.query, now)
			if tc.wantStatus != 0 {
				require.NotNil(t, searchErr)
				assert.Equal(t, tc.wantStatus, searchErr.Status)
				return
			}
			require.Nil(t, searchErr)
			assert.Equal(t, tc.want, got)
		})
	}
}

// 元宝请求：默认地址、Bearer key，体只含 Query/Site/Freshness，从不发 Cnt。
func TestTencentWSABuildRequest(t *testing.T) {
	provider := tencentWSAProvider{now: time.Now}
	req, searchErr := provider.BuildRequest(context.Background(), "", "wsa-key", SearchRequest{Query: "南极 site:81.cn", Count: 3})
	require.Nil(t, searchErr)
	assert.Equal(t, "https://api.wsa.cloud.tencent.com/SearchPro", req.URL.String())
	assert.Equal(t, "Bearer wsa-key", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Query":"南极","Site":"81.cn"}`, string(body))
}

const tencentWSASample = `{"Response":{"RequestId":"r-1","Query":"南极","Pages":[` +
	`"{\"title\":\"南极冰下\",\"url\":\"https://www.81.cn/yw_208727/16492942.html\",\"date\":\"2026/10/14 06:56:39\",\"passage\":\"冰下湖泊\",\"site\":\"中国军网\",\"favicon\":\"https://www.81.cn/favicon.ico\"}",` +
	`"{not json",` +
	`42,` +
	`"{\"title\":\"科考\",\"url\":\"https://example.cn/2\",\"date\":\"昨天\",\"passage\":\"段落\",\"content\":\"动态摘要\",\"site\":\"示例\"}",` +
	`"{\"title\":\"第三条\",\"url\":\"https://example.cn/3\",\"passage\":\"p3\",\"site\":\"示例\"}"` +
	`],"Version":"lite"}}`

func TestTencentWSAParseResponse(t *testing.T) {
	provider := tencentWSAProvider{now: time.Now}
	got, searchErr := provider.ParseResponse(context.Background(), http.StatusOK, []byte(tencentWSASample), SearchRequest{Query: "南极", Count: 2})
	require.Nil(t, searchErr)
	assert.Equal(t, []SearchResult{
		{Title: "南极冰下", URL: "https://www.81.cn/yw_208727/16492942.html", Snippet: "冰下湖泊", Summary: "冰下湖泊", SiteName: "中国军网", Date: "2026-10-14T06:56:39+08:00"},
		{Title: "科考", URL: "https://example.cn/2", Snippet: "段落", Summary: "动态摘要", SiteName: "示例"},
	}, got.Results, "bad pages are dropped, date falls back to omitted, results stop at count")

	got, searchErr = provider.ParseResponse(context.Background(), http.StatusOK, []byte(`{"Response":{"RequestId":"r-2","Pages":[]}}`), SearchRequest{Query: "南极", Count: 8})
	require.Nil(t, searchErr)
	encoded, err := common.Marshal(got)
	require.NoError(t, err)
	assert.Equal(t, `{"results":[]}`, string(encoded))
}

func TestTencentWSAErrorStatuses(t *testing.T) {
	tests := []struct {
		name, body string
		status     int
		want       int
	}{
		{"invalid parameter", `{"Response":{"Error":{"Code":"InvalidParameter.Query","Message":"x"},"RequestId":"r"}}`, http.StatusOK, http.StatusBadRequest},
		{"unauthorized", `{"Response":{"Error":{"Code":"UnauthorizedOperation","Message":"未授权操作。"},"RequestId":"a546"}}`, http.StatusOK, http.StatusUnauthorized},
		{"auth failure", `{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"x"}}}`, http.StatusOK, http.StatusUnauthorized},
		{"not opened", `{"Response":{"Error":{"Code":"ResourceNotFound","Message":"x"}}}`, http.StatusOK, http.StatusPaymentRequired},
		{"in arrears", `{"Response":{"Error":{"Code":"ResourceUnavailable.InArrears","Message":"x"}}}`, http.StatusOK, http.StatusPaymentRequired},
		{"insufficient", `{"Response":{"Error":{"Code":"ResourceInsufficient","Message":"x"}}}`, http.StatusOK, http.StatusPaymentRequired},
		{"quota limit", `{"Response":{"Error":{"Code":"LimitExceeded","Message":"x"}}}`, http.StatusOK, http.StatusPaymentRequired},
		{"rate limit", `{"Response":{"Error":{"Code":"RequestLimitExceeded","Message":"x"}}}`, http.StatusOK, http.StatusTooManyRequests},
		{"internal", `{"Response":{"Error":{"Code":"InternalError","Message":"x"}}}`, http.StatusOK, http.StatusBadGateway},
		{"neither error nor pages", `{"Response":{"RequestId":"r"}}`, http.StatusOK, http.StatusBadGateway},
		{"not json", `<html>`, http.StatusOK, http.StatusBadGateway},
		{"http 401", ``, http.StatusUnauthorized, http.StatusUnauthorized},
		{"http 500", ``, http.StatusInternalServerError, http.StatusBadGateway},
	}
	provider := tencentWSAProvider{now: time.Now}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, searchErr := provider.ParseResponse(context.Background(), tc.status, []byte(tc.body), SearchRequest{Query: "q", Count: 8})
			require.NotNil(t, searchErr)
			assert.Equal(t, tc.want, searchErr.Status)
		})
	}
}

func TestDeepSeekParseResponse(t *testing.T) {
	provider := deepSeekProvider{}
	body := `{"results":[` +
		`{"title":"DeepSeek","url":"https://deepseek.com/","snippet":"s","summary":"m","site_name":"DeepSeek","date":"2026-10-14T06:56:39+08:00","rerank_score":2.1},` +
		`{"title":"Two","url":"https://two.example/","date":1760000000},` +
		`{"title":"Three","url":"https://three.example/"}]}`
	got, searchErr := provider.ParseResponse(context.Background(), http.StatusOK, []byte(body), SearchRequest{Query: "q", Count: 2})
	require.Nil(t, searchErr)
	assert.Equal(t, []SearchResult{
		{Title: "DeepSeek", URL: "https://deepseek.com/", Snippet: "s", Summary: "m", SiteName: "DeepSeek", Date: "2026-10-14T06:56:39+08:00"},
		{Title: "Two", URL: "https://two.example/"},
	}, got.Results)

	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusBadRequest, `{"error":{"message":"bad query"}}`, http.StatusBadRequest},
		{http.StatusUnauthorized, ``, http.StatusUnauthorized},
		{http.StatusForbidden, ``, http.StatusUnauthorized},
		{http.StatusPaymentRequired, ``, http.StatusPaymentRequired},
		{http.StatusTooManyRequests, ``, http.StatusTooManyRequests},
		{http.StatusServiceUnavailable, ``, http.StatusBadGateway},
		{http.StatusNotFound, ``, http.StatusBadGateway},
		{http.StatusOK, `{"results":null}`, http.StatusBadGateway},
		{http.StatusOK, `{"data":[]}`, http.StatusBadGateway},
	} {
		_, searchErr := provider.ParseResponse(context.Background(), tc.status, []byte(tc.body), SearchRequest{Query: "q", Count: 8})
		require.NotNil(t, searchErr, tc.status)
		assert.Equal(t, tc.want, searchErr.Status, "%d %s", tc.status, tc.body)
	}
}

// Tavily 运算符翻译：全部 site:/-site: 都保留，日期原样转起止日，无效日期留作查询词。
func TestTranslateTavilyQuery(t *testing.T) {
	tests := []struct {
		name, query string
		want        tavilyRequest
		wantStatus  int
	}{
		{name: "plain text", query: "南极  冰下", want: tavilyRequest{Query: "南极 冰下"}},
		{name: "all sites and exclusions", query: "site:a.com 南极 site:b.com -site:c.com -site:d.com",
			want: tavilyRequest{Query: "南极", IncludeDomains: []string{"a.com", "b.com"}, ExcludeDomains: []string{"c.com", "d.com"}}},
		{name: "dates", query: "after:2026-10-01 南极 before:2026-10-31 lang:zh",
			want: tavilyRequest{Query: "南极", StartDate: "2026-10-01", EndDate: "2026-10-31"}},
		{name: "malformed date stays as text", query: "after:2026/10/01 南极", want: tavilyRequest{Query: "after:2026/10/01 南极"}},
		{name: "quoted phrase kept", query: `"site:x 冰下" 湖泊`, want: tavilyRequest{Query: `"site:x 冰下" 湖泊`}},
		{name: "only operators", query: "site:a.com before:2026-10-31", wantStatus: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, searchErr := translateTavilyQuery(SearchRequest{Query: tc.query, Count: 5})
			if tc.wantStatus != 0 {
				require.NotNil(t, searchErr)
				assert.Equal(t, tc.wantStatus, searchErr.Status)
				return
			}
			require.Nil(t, searchErr)
			tc.want.MaxResults, tc.want.SearchDepth, tc.want.IncludePublishedDate = 5, "basic", true
			assert.Equal(t, tc.want, got)
		})
	}
}

// Tavily 请求：渠道地址优先、Bearer key，固定 basic 深度并要发布日期，count 直接作 max_results。
func TestTavilyBuildRequest(t *testing.T) {
	req, searchErr := tavilyProvider{}.BuildRequest(context.Background(), "https://proxy.example/", "tvly-key", SearchRequest{Query: "南极 site:81.cn after:2026-10-01", Count: 3})
	require.Nil(t, searchErr)
	assert.Equal(t, "https://proxy.example/search", req.URL.String())
	assert.Equal(t, "Bearer tvly-key", req.Header.Get("Authorization"))
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.JSONEq(t, `{"query":"南极","max_results":3,"search_depth":"basic","include_published_date":true,"include_domains":["81.cn"],"start_date":"2026-10-01"}`, string(body))

	req, searchErr = tavilyProvider{}.BuildRequest(context.Background(), "", "tvly-key", SearchRequest{Query: "南极", Count: 3})
	require.Nil(t, searchErr)
	assert.Equal(t, "https://api.tavily.com/search", req.URL.String())
}

func TestTavilyParseResponse(t *testing.T) {
	body := `{"query":"q","results":[` +
		`{"title":"销量","url":"https://www.36kr.com/p/1","content":"摘要一","score":0.9,"published_date":"Mon, 01 Dec 2025 00:00:00 GMT"},` +
		`{"title":"二","url":"https://finance.sina.com.cn/2","content":"摘要二","published_date":null},` +
		`{"title":"三","url":"https://three.example/","content":"c"}],"usage":{"credits":1}}`
	got, searchErr := tavilyProvider{}.ParseResponse(context.Background(), http.StatusOK, []byte(body), SearchRequest{Query: "q", Count: 2})
	require.Nil(t, searchErr)
	assert.Equal(t, []SearchResult{
		{Title: "销量", URL: "https://www.36kr.com/p/1", Snippet: "摘要一", Summary: "摘要一", SiteName: "36kr.com", Date: "2025-12-01T00:00:00Z"},
		{Title: "二", URL: "https://finance.sina.com.cn/2", Snippet: "摘要二", Summary: "摘要二", SiteName: "finance.sina.com.cn"},
	}, got.Results)

	got, searchErr = tavilyProvider{}.ParseResponse(context.Background(), http.StatusOK, []byte(`{"results":[]}`), SearchRequest{Query: "q", Count: 8})
	require.Nil(t, searchErr)
	encoded, err := common.Marshal(got)
	require.NoError(t, err)
	assert.Equal(t, `{"results":[]}`, string(encoded))
}

func TestTavilyErrorStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{http.StatusBadRequest, `{"detail":{"error":"bad query"}}`, http.StatusBadRequest},
		{http.StatusUnauthorized, `{"detail":{"error":"Unauthorized: missing or invalid API key."}}`, http.StatusUnauthorized},
		{http.StatusForbidden, ``, http.StatusUnauthorized},
		{432, `{"detail":{"error":"This request exceeds your plan's set usage limit."}}`, http.StatusPaymentRequired},
		{433, `{"detail":{"error":"This request exceeds the pay-as-you-go limit."}}`, http.StatusPaymentRequired},
		{http.StatusTooManyRequests, ``, http.StatusTooManyRequests},
		{http.StatusInternalServerError, ``, http.StatusBadGateway},
		{http.StatusOK, `{"results":null}`, http.StatusBadGateway},
		{http.StatusOK, `<html>`, http.StatusBadGateway},
	} {
		_, searchErr := tavilyProvider{}.ParseResponse(context.Background(), tc.status, []byte(tc.body), SearchRequest{Query: "q", Count: 8})
		require.NotNil(t, searchErr, tc.status)
		assert.Equal(t, tc.want, searchErr.Status, "%d %s", tc.status, tc.body)
	}
}
