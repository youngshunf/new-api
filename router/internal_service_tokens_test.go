package router

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 内部通道凭据在 router 包里**只能由这里装载一次**。
//
// # 为什么必须收口到一个地方
//
// `middleware.loadInternalServiceTokens` 带 `sync.Once`：一个测试进程只装载一次凭据表，
// 之后再改 `INTERNAL_SERVICE_TOKENS` 完全无效。于是两个用例各自 `t.Setenv` 各自那套 scope 时，
// **先跑的那个赢，后跑的静默拿到别人的凭据表**——表现是「我明明配了 llm 凭据，却收到 401」。
//
// 这在 S1-B/C 与 commerce N1 合并的那一刻真实发生过：N1 的用例先跑并装载了
// `credit + account`，LLM 的用例随后就再也读不到自己的 `llm` 凭据。两个分支各自全绿，
// 合起来才红——而红的样子长得像鉴权 bug，不像测试互相污染。
//
// 收口的办法是**一份凭据表覆盖全部 scope**，任何 scope 的路由用例都从这里取 token。
// 新增 scope 时在 `internalServiceTestScopes` 里加一项，不要新写一个 `t.Setenv`。
var internalServiceTestScopes = []string{"credit", "llm", "account"}

// 供 router 包全部内部通道用例使用的凭据表。返回 scope → token。
//
// 它可以被多个用例重复调用：`sync.Once` 保证只有第一次真正生效，而因为每次给的是**同一张
// 完整的表**，谁先跑都不影响结果——这正是收口要达到的性质。
func internalServiceTestCredentials(t *testing.T) map[string]string {
	t.Helper()
	tokens := make(map[string]string, len(internalServiceTestScopes))
	entries := make([]string, 0, len(internalServiceTestScopes))
	for _, scope := range internalServiceTestScopes {
		token := testRouterServiceToken(scope)
		tokens[scope] = token
		entries = append(entries, scope+":"+token)
	}
	t.Setenv("INTERNAL_SERVICE_TOKENS", strings.Join(entries, ","))
	return tokens
}

// 自反守卫：router 包里除了本文件，任何 `_test.go` 都不得自己设 `INTERNAL_SERVICE_TOKENS`。
//
// 少了这条，上面那段头注就只是一句劝告——而这个坑的代价是「两个分支各自全绿、合起来才红」，
// 靠人记住是不够的。
func TestOnlyOneRouterTestLoadsInternalServiceTokens(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取包目录失败：%v", err)
	}
	// 刻意用拼接构造 needle：写成字面量会命中本文件自己，守卫从此恒绿。
	needle := regexp.MustCompile(`Setenv\(\s*"INTERNAL_SERVICE` + `_TOKENS"`)

	var offenders []string
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") || name == "internal_service_tokens_test.go" {
			continue
		}
		body, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatalf("读取 %s 失败：%v", name, readErr)
		}
		scanned++
		if needle.Match(body) {
			offenders = append(offenders, name)
		}
	}

	if scanned == 0 {
		t.Fatal("一个 _test.go 都没扫到——判定面为空，这条守卫结构上不可能发现问题")
	}
	if len(offenders) > 0 {
		t.Fatalf("这些用例自己设了 INTERNAL_SERVICE_TOKENS，会与别的 scope 用例互相覆盖："+
			"%v；改用 internalServiceTestCredentials(t)（本文件头注说明了为什么）", offenders)
	}
}
