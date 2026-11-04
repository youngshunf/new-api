package websearch

import (
	"net/http"
	"strings"
	"time"
)

// searchOperatorDateLayout 是 after:/before: 唯一认得的日期写法。
const searchOperatorDateLayout = "2006-01-02"

// searchOperators 是从统一查询串取出的搜索运算符；各提供方再翻成自己的参数。
type searchOperators struct {
	// Text 是去掉运算符后的查询词，单空格连接，恒非空。
	Text string
	// Sites 是全部 site: 值、Excludes 是全部 -site: 值，按出现顺序。
	Sites    []string
	Excludes []string
	// After、Before 是第一个合法的 after:/before: 日期（YYYY-MM-DD），没有为空。
	After  string
	Before string
}

// parseSearchOperators 从查询串取出 site:、-site:、after:、before:，删去 lang:。
// 引号短语内的词不当运算符；没有值的运算符、日期不是 YYYY-MM-DD 的 after:/before: 当普通文本
// （不回 400：400 不换渠道，别的提供方可能认得这种写法）。余下文本为空时返回 400。
func parseSearchOperators(query string) (searchOperators, *SearchError) {
	var operators searchOperators
	var text []string
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
			operators.Excludes = append(operators.Excludes, value)
			continue
		}
		if value, ok := strings.CutPrefix(token, "site:"); ok && value != "" {
			operators.Sites = append(operators.Sites, value)
			continue
		}
		if value, ok := cutDateOperator(token, "after:"); ok {
			if operators.After == "" {
				operators.After = value
			}
			continue
		}
		if value, ok := cutDateOperator(token, "before:"); ok {
			if operators.Before == "" {
				operators.Before = value
			}
			continue
		}
		if value, ok := strings.CutPrefix(token, "lang:"); ok && value != "" {
			continue
		}
		text = append(text, token)
	}
	operators.Text = strings.Join(text, " ")
	if operators.Text == "" {
		return searchOperators{}, newSearchError(http.StatusBadRequest, "query has no search text besides operators")
	}
	return operators, nil
}

// cutDateOperator 取出 prefix 后的 YYYY-MM-DD 日期；不是这种写法时返回 false，由调用方当普通文本。
func cutDateOperator(token, prefix string) (string, bool) {
	value, ok := strings.CutPrefix(token, prefix)
	if !ok {
		return "", false
	}
	if _, err := time.Parse(searchOperatorDateLayout, value); err != nil {
		return "", false
	}
	return value, true
}
