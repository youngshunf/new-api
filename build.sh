#!/bin/bash
# new-api 编译脚本（合并上游 v1.0 双前端架构后适用）
# 用法: ./build.sh [--skip-frontend] [--no-classic] [--target linux|darwin] [--arch amd64|arm64]
#
# 注意：上游运行时默认 theme=classic，所以默认必须把 web/classic 真实构建出来，
#       否则 binary 启动后访问根路径会拿到 build placeholder 空白页。
#       要切到 v1.0 新前端：登 admin → System Settings → Frontend → default。

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# 默认参数
SKIP_FRONTEND=false
BUILD_CLASSIC=true
TARGET_OS=""
TARGET_ARCH="amd64"
OUTPUT_NAME="new-api"

# 解析参数
while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-frontend)
            SKIP_FRONTEND=true
            shift
            ;;
        --no-classic)
            BUILD_CLASSIC=false
            shift
            ;;
        --target)
            TARGET_OS="$2"
            shift 2
            ;;
        --arch)
            TARGET_ARCH="$2"
            shift 2
            ;;
        --help|-h)
            echo "用法: ./build.sh [选项]"
            echo ""
            echo "选项:"
            echo "  --skip-frontend     跳过前端构建（使用已有的 dist，缺失时建占位）"
            echo "  --no-classic        跳过 web/classic（默认两份都建；上游运行时默认 theme=classic）"
            echo "  --target OS         交叉编译目标系统（linux/darwin），默认当前系统"
            echo "  --arch ARCH         目标架构（amd64/arm64），默认 amd64"
            echo "  -h, --help          显示帮助"
            echo ""
            echo "前端架构（上游 v1.0 后）:"
            echo "  web/default/        新前端（React 19 + Rsbuild + Base UI），含 huanxing admin-tokens 移植"
            echo "  web/classic/        旧前端（React 18 + Vite + Semi Design），huanxing token UI 完整保留"
            echo "  main.go 用 //go:embed web/{default,classic}/dist 嵌入两份"
            echo "  上游运行时默认 theme=classic，admin 在 System Settings → Frontend 可切到 default"
            echo ""
            echo "示例:"
            echo "  ./build.sh                              # 完整构建（default + classic + 后端），当前平台"
            echo "  ./build.sh --no-classic                 # 仅 default 前端 + 后端（确认全用 default theme 时）"
            echo "  ./build.sh --skip-frontend              # 仅编译 Go 后端"
            echo "  ./build.sh --target linux               # 交叉编译 Linux amd64（含两套前端）"
            echo "  ./build.sh --target linux --arch arm64  # 交叉编译 Linux arm64"
            exit 0
            ;;
        *)
            echo "未知参数: $1"
            exit 1
            ;;
    esac
done

echo "========================================="
echo "  new-api 编译脚本"
echo "========================================="

# ===== 占位 dist 工具（保证 //go:embed 不会失败）=====
ensure_placeholder_dist() {
    local dir="$1"
    if [ ! -d "$dir" ]; then
        mkdir -p "$dir"
        echo "<!doctype html><html><body>placeholder</body></html>" > "$dir/index.html"
    elif [ ! -f "$dir/index.html" ]; then
        # 目录存在但没 index.html 也会让 //go:embed dir/index.html 失败
        echo "<!doctype html><html><body>placeholder</body></html>" > "$dir/index.html"
    fi
}

# ========== 1. 前端构建 ==========
if [ "$SKIP_FRONTEND" = false ]; then
    echo ""
    echo "[1/2] 构建前端..."

    if ! command -v bun &> /dev/null; then
        echo "错误: 未找到 bun，请先安装: curl -fsSL https://bun.sh/install | bash"
        exit 1
    fi

    echo "  → web/default ..."
    (cd web/default && (bun install --frozen-lockfile 2>/dev/null || bun install))
    (cd web/default && bun run build)
    if [ ! -d "web/default/dist" ]; then
        echo "错误: web/default/dist 不存在，前端构建失败"
        exit 1
    fi

    if [ "$BUILD_CLASSIC" = true ]; then
        echo "  → web/classic ..."
        (cd web/classic && (bun install --frozen-lockfile 2>/dev/null || bun install))
        (cd web/classic && bun run build)
        if [ ! -d "web/classic/dist" ]; then
            echo "错误: web/classic/dist 不存在，前端构建失败"
            exit 1
        fi
    else
        # main.go 中 //go:embed web/classic/dist 必须有目标，否则 go build 失败
        ensure_placeholder_dist "web/classic/dist"
        echo "  → web/classic（占位，--no-classic 已开；上游默认 theme=classic，需到 System Settings 切到 default）"
    fi
    echo "  前端构建完成 ✓"
else
    echo ""
    echo "[1/2] 跳过前端构建"
    # //go:embed 目标必须存在，缺失时建占位
    ensure_placeholder_dist "web/default/dist"
    ensure_placeholder_dist "web/classic/dist"
fi

# ========== 2. Go 后端编译 ==========
echo ""
echo "[2/2] 编译 Go 后端..."

# 设置 Go 代理（国内加速；外部 env 已设则不覆盖）
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

# 交叉编译设置
if [ -n "$TARGET_OS" ]; then
    export GOOS="$TARGET_OS"
    export GOARCH="$TARGET_ARCH"
    OUTPUT_NAME="new-api-${GOOS}-${GOARCH}"
    echo "  交叉编译: ${GOOS}/${GOARCH}"
fi

# 编译
echo "  编译中..."
go build -o "$OUTPUT_NAME" -ldflags "-s -w" .

if [ $? -eq 0 ]; then
    FILE_SIZE=$(du -h "$OUTPUT_NAME" | cut -f1)
    echo "  编译完成 ✓"
    echo ""
    echo "========================================="
    echo "  输出文件: ${SCRIPT_DIR}/${OUTPUT_NAME}"
    echo "  文件大小: ${FILE_SIZE}"
    if [ -n "$TARGET_OS" ]; then
        echo "  目标平台: ${GOOS}/${GOARCH}"
    fi
    echo "========================================="
    echo ""
    echo "启动命令:"
    echo "  SQL_DSN=\"postgres://user:pass@127.0.0.1:5432/huanxing\" \\"
    echo "  PORT=3000 \\"
    echo "  SESSION_SECRET=\$(openssl rand -hex 16) \\"
    echo "  ./${OUTPUT_NAME}"
    echo ""
    echo "本地一键起：./dev-local.sh"
else
    echo "  编译失败 ✗"
    exit 1
fi
