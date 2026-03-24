#!/bin/bash
# new-api 一键编译部署脚本
# 用法: ./deploy.sh [--skip-frontend]

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# ========== 配置（换服务器改这里）==========
SERVER_HOST="117.72.92.229"
SERVER_USER="root"
DEPLOY_DIR="/www/wwwroot/llm.dcfuture.cn"
SERVICE_NAME="llm.dcfuture.cn"
HEALTH_PORT="3180"
# ==========================================

SKIP_FRONTEND=false
BINARY="new-api-linux-amd64"

while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-frontend) SKIP_FRONTEND=true; shift ;;
        -h|--help)
            echo "用法: ./deploy.sh [--skip-frontend]"
            echo "  --skip-frontend  跳过前端构建，仅重新编译 Go 后端"
            exit 0 ;;
        *) echo "未知参数: $1"; exit 1 ;;
    esac
done

echo "========================================="
echo "  new-api 编译部署"
echo "  服务器: ${SERVER_USER}@${SERVER_HOST}"
echo "  目录:   ${DEPLOY_DIR}"
echo "========================================="

# ========== 1. 编译 ==========
BUILD_ARGS="--target linux"
[ "$SKIP_FRONTEND" = true ] && BUILD_ARGS="$BUILD_ARGS --skip-frontend"

echo ""
echo "[1/3] 编译..."
./build.sh $BUILD_ARGS

# ========== 2. 上传 ==========
echo ""
echo "[2/3] 上传到服务器..."
scp "$BINARY" "${SERVER_USER}@${SERVER_HOST}:${DEPLOY_DIR}/new-api.new"
echo "  上传完成 ✓"

# ========== 3. 部署重启 ==========
echo ""
echo "[3/3] 部署并重启服务..."
ssh "${SERVER_USER}@${SERVER_HOST}" bash <<EOF
set -e
cd "${DEPLOY_DIR}"
chmod +x new-api.new
supervisorctl stop "${SERVICE_NAME}"
cp new-api new-api.bak.\$(date +%Y%m%d%H%M%S)
mv new-api.new new-api
supervisorctl start "${SERVICE_NAME}"
sleep 3
supervisorctl status "${SERVICE_NAME}"
EOF

# ========== 4. 健康检查 ==========
echo ""
echo "健康检查..."
if ssh "${SERVER_USER}@${SERVER_HOST}" "curl -sf http://localhost:${HEALTH_PORT}/api/status > /dev/null"; then
    echo "  服务响应正常 ✓"
else
    echo "  ⚠ 健康检查未通过，请查看日志："
    echo "    ssh ${SERVER_USER}@${SERVER_HOST} 'tail -50 ${DEPLOY_DIR}/logs/stderr.log'"
    exit 1
fi

echo ""
echo "========================================="
echo "  部署完成"
echo "========================================="
