#!/usr/bin/env bash
# new-api 本地编译启动脚本（上游 2026-08 退役 classic 前端后适用）
#
# 默认行为：
#   1) 构建 web（唯一前端，React 19 + Rsbuild + Base UI，含 huanxing admin-tokens 定制）
#   2) Go 编译为 ./new-api
#   3) 直接启动；运行时配置由 main.go 的 godotenv.Load(".env") 读 ./.env
#
# 用法:
#   ./dev-local.sh                       # 默认：建前端 + 后端，启动
#   ./dev-local.sh --skip-frontend       # 跳过前端，仅重新 go build + 启动
#   ./dev-local.sh --no-start            # 只编译，不启动
#
# 运行时配置一律走 ./.env（git-ignored）：
#   PORT=3000
#   SQL_DSN=postgres://...               # 留空 = SQLite，路径见 SQLITE_PATH
#   SESSION_SECRET=...                   # 不设会随机生成（每次启动会让现有会话失效）
#   ...                                  # 其它见 .env.example
#
# Go 代理用环境 GOPROXY，外部 export 即可；脚本不会强制覆盖。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# ===== 默认参数 =====
SKIP_FRONTEND=false
NO_START=false

# ===== 参数解析 =====
while [[ $# -gt 0 ]]; do
    case $1 in
        # 兼容旧调用方：classic 前端已随上游退役，这个开关不再有任何作用
        --no-classic)     echo "提示: --no-classic 已废弃（classic 前端已退役），忽略该参数"; shift ;;
        --skip-frontend)  SKIP_FRONTEND=true; shift ;;
        --no-start)       NO_START=true; shift ;;
        -h|--help)
            sed -n '2,19p' "$0"
            exit 0
            ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

echo "============================================"
echo "  new-api 本地编译启动"
if [ "$SKIP_FRONTEND" = true ]; then
    echo "  前端：(跳过)"
else
    echo "  前端：web"
fi
echo "  运行时配置：./.env"
[ -f .env ] || echo "  ⚠ .env 不存在，建议 cp .env.example .env 后按需填值"
echo "============================================"

# ===== 依赖检查 =====
need() { command -v "$1" >/dev/null 2>&1 || { echo "错误: 未找到 $1，请先安装"; exit 1; }; }
need go
[ "$SKIP_FRONTEND" = true ] || need bun

# ===== 1. 前端构建 =====
ensure_placeholder_dist() {
    local dir="$1"
    if [ ! -d "$dir" ]; then
        mkdir -p "$dir"
        echo "<!doctype html><html><body>placeholder</body></html>" > "$dir/index.html"
    elif [ ! -f "$dir/index.html" ]; then
        echo "<!doctype html><html><body>placeholder</body></html>" > "$dir/index.html"
    fi
}

if [ "$SKIP_FRONTEND" = false ]; then
    echo ""
    echo "[1/2] 构建前端..."

    echo "  → web ..."
    (cd web && (bun install --frozen-lockfile 2>/dev/null || bun install))
    (cd web && bun run build)
    [ -d web/dist ] || { echo "错误: web/dist 不存在"; exit 1; }
    echo "  前端构建完成 ✓"
else
    echo ""
    echo "[1/2] 跳过前端构建"
    # main.go 中 //go:embed web/dist 必须有目标，否则编译失败
    ensure_placeholder_dist "web/dist"
fi

# ===== 2. Go 编译 =====
echo ""
echo "[2/2] 编译 Go 后端..."
go build -o new-api -ldflags "-s -w" .
echo "  编译完成 ✓ ($(du -h new-api | cut -f1))"

# ===== 3. 启动 =====
if [ "$NO_START" = true ]; then
    echo ""
    echo "已编译，未启动（--no-start）"
    echo "手动启动：./new-api"
    exit 0
fi

mkdir -p logs
echo ""
echo "============================================"
echo "  启动 new-api（配置来自 ./.env）"
echo "  日志：./logs/new-api-$(date +%Y%m%d).log（同步终端输出）"
echo "  停止：Ctrl+C"
echo "============================================"
echo ""

./new-api 2>&1 | tee -a "logs/new-api-$(date +%Y%m%d).log"
