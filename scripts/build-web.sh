#!/usr/bin/env bash
# 构建前端并把产物写入 internal/web/dist（供 go:embed 使用）。
# 在无 node 的环境下可跳过：仓库已提交构建产物，go build 仍可单独工作。
set -euo pipefail
cd "$(dirname "$0")/.."
cd frontend
if [ ! -d node_modules ]; then
  npm ci --no-audit --no-fund
fi
npm run build
echo "frontend built -> internal/web/dist"
