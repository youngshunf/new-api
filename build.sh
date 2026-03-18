#!/bin/bash
# new-api 编译脚本
# 用法: ./build.sh [--skip-frontend] [--target linux|darwin]

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# 默认参数
SKIP_FRONTEND=false
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
            echo "  --skip-frontend    跳过前端构建（使用已有的 web/dist）"
            echo "  --target OS        交叉编译目标系统（linux/darwin），默认当前系统"
            echo "  --arch ARCH        目标架构（amd64/arm64），默认 amd64"
            echo "  -h, --help         显示帮助"
            echo ""
            echo "示例:"
            echo "  ./build.sh                          # 完整构建（前端+后端），当前平台"
            echo "  ./build.sh --skip-frontend           # 仅编译 Go 后端"
            echo "  ./build.sh --target linux             # 交叉编译 Linux amd64"
            echo "  ./build.sh --target linux --arch arm64 # 交叉编译 Linux arm64"
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

# ========== 1. 前端构建 ==========
if [ "$SKIP_FRONTEND" = false ]; then
    echo ""
    echo "[1/2] 构建前端..."

    if ! command -v bun &> /dev/null; then
        echo "错误: 未找到 bun，请先安装: curl -fsSL https://bun.sh/install | bash"
        exit 1
    fi

    cd web
    echo "  安装依赖..."
    bun install --frozen-lockfile 2>/dev/null || bun install
    echo "  构建生产版本..."
    bun run build
    cd "$SCRIPT_DIR"

    if [ ! -d "web/dist" ]; then
        echo "错误: 前端构建失败，web/dist 目录不存在"
        exit 1
    fi
    echo "  前端构建完成 ✓"
else
    echo ""
    echo "[1/2] 跳过前端构建"
    if [ ! -d "web/dist" ]; then
        echo "警告: web/dist 不存在，Go embed 会失败"
        echo "  创建空的 dist 目录和 index.html..."
        mkdir -p web/dist
        echo "<html><body>前端未构建</body></html>" > web/dist/index.html
    fi
fi

# ========== 2. Go 后端编译 ==========
echo ""
echo "[2/2] 编译 Go 后端..."

# 设置 Go 代理（国内加速）
export GOPROXY=https://goproxy.cn,direct

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
else
    echo "  编译失败 ✗"
    exit 1
fi
