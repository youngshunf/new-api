package controller

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 上游以 200 + Content-Type: video/mp4 把文本错误体原样回来时，代理必须判出它不是视频，
// 否则客户端会把这段 JSON 入桶、当成出片成功，主人拿到一个打不开的产物。
func TestLooksLikeUpstreamErrorBody(t *testing.T) {
	// 一个最小但合法的 MP4 头：前 4 字节是 box 长度，紧接着 `ftyp`。
	mp4Head := append([]byte{0x00, 0x00, 0x00, 0x18}, []byte("ftypisom")...)
	mp4Head = append(mp4Head, bytes.Repeat([]byte{0x00}, 64)...)

	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{
			// 生产实测的 22 字节回包（渠道内部转发到 LiteLLM，路由信息缺失）。
			name: "上游 JSON 错误体",
			body: []byte(`{"detail":"Not Found"}`),
			want: true,
		},
		{
			name: "带前导空白的 JSON 错误体",
			body: []byte("\n  {\"error\":{\"message\":\"gone\"}}\n"),
			want: true,
		},
		{
			name: "网关 HTML 错误页",
			body: []byte("<html><body>502 Bad Gateway</body></html>"),
			want: true,
		},
		{
			name: "真实 MP4 出片放行",
			body: mp4Head,
			want: false,
		},
		{
			name: "WebM/MKV 魔数放行",
			body: append([]byte{0x1A, 0x45, 0xDF, 0xA3}, bytes.Repeat([]byte{0x42}, 64)...),
			want: false,
		},
		{
			// 认不出的容器格式不等于不是视频：非 UTF-8 一律放行。
			name: "非 UTF-8 二进制放行",
			body: []byte{0xFF, 0xFE, 0x00, 0x01, 0x02, 0x03},
			want: false,
		},
		{
			// 这条专门钉住 utf8.Valid 那道检查：首字节是 `{` 已经骗过首字符判据，
			// 只有 UTF-8 校验能把它拦下来。去掉那道检查本条立刻变红。
			name: "花括号开头但含非法 UTF-8 放行",
			body: []byte{'{', 0xFF, 0xFE, '}'},
			want: false,
		},
		{
			name: "空响应体放行",
			body: nil,
			want: false,
		},
		{
			name: "只有空白放行",
			body: []byte("   \n\t "),
			want: false,
		},
		{
			// 读满预读缓冲说明后面还有内容，不可能是一小段错误 JSON。
			name: "达到预读上限放行",
			body: append([]byte("{"), bytes.Repeat([]byte("a"), videoErrorBodySniffLimit)...),
			want: false,
		},
		{
			// box 长度恰好等于 '{' 的 MP4：首字符判据挡不住，靠 ftyp 兜住。
			name: "长度字段恰为花括号的 MP4 放行",
			body: append([]byte{'{', 0x00, 0x00, 0x18}, []byte("ftypisom")...),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, looksLikeUpstreamErrorBody(tc.body))
		})
	}
}
