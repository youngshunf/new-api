#!/usr/bin/env bash
# new-api 本地编译启动脚本（合并上游 v1.0 双前端架构后适用）
#
# 默认行为：
#   1) 构建 web/default（v1.0 新前端，含 huanxing admin-tokens 移植）
#      和 web/classic（上游默认 theme=classic，必须真实构建否则空白页）
#   2) Go 编译为 ./new-api
#   3) 直接启动；运行时配置由 main.go 的 godotenv.Load(".env") 读 ./.env
#
# 用法:
#   ./dev-local.sh                       # 默认：default + classic 两份都建，启动
#   ./dev-local.sh --no-classic          # 只建 default（用 default theme 时省时间）
#   ./dev-local.sh --skip-frontend       # 跳过前端，仅重新 go build + 启动
#   ./dev-local.sh --no-start            # 只编译，不启动
#
# 切换前端 theme：登 admin → System Settings → Frontend，default ↔ classic
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
BUILD_CLASSIC=true
SKIP_FRONTEND=false
NO_START=false

# ===== 参数解析 =====
while [[ $# -gt 0 ]]; do
    case $1 in
        --no-classic)     BUILD_CLASSIC=false; shift ;;
        --skip-frontend)  SKIP_FRONTEND=true; shift ;;
        --no-start)       NO_START=true; shift ;;
        -h|--help)
            sed -n '2,22p' "$0"
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
    echo "  前端：default $([ "$BUILD_CLASSIC" = true ] && echo "+ classic")"
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

    echo "  → web/default ..."
    (cd web/default && (bun install --frozen-lockfile 2>/dev/null || bun install))
    (cd web/default && bun run build)
    [ -d web/default/dist ] || { echo "错误: web/default/dist 不存在"; exit 1; }

    if [ "$BUILD_CLASSIC" = true ]; then
        echo "  → web/classic ..."
        (cd web/classic && (bun install --frozen-lockfile 2>/dev/null || bun install))
        (cd web/classic && bun run build)
        [ -d web/classic/dist ] || { echo "错误: web/classic/dist 不存在"; exit 1; }
    else
        # main.go 中 //go:embed web/classic/dist 必须有目标，否则编译失败
        ensure_placeholder_dist "web/classic/dist"
        echo "  → web/classic（占位，--no-classic 已开；上游默认 theme=classic，请到 System Settings 切到 default）"
    fi
    echo "  前端构建完成 ✓"
else
    echo ""
    echo "[1/2] 跳过前端构建"
    ensure_placeholder_dist "web/default/dist"
    ensure_placeholder_dist "web/classic/dist"
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
