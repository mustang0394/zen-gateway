# zen-gateway 构建入口
#
#   make web      构建前端（输出到 internal/web/dist，由 go:embed 打包）
#   make build    编译二进制（含前端资源）
#   make test     运行全部测试（含 -race）
#   make docker   构建 Docker 镜像
#   make dev      起前端开发服务器（代理到本地 8080）
.PHONY: web build test vet docker dev clean

BINARY := zen-gateway
FRONTEND := frontend
DIST := internal/web/dist

web:
	cd $(FRONTEND) && npm ci --no-audit --no-fund && npm run build

build: web
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/gateway

build-go:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/gateway

test:
	go test -race ./...

vet:
	go vet ./...

docker:
	docker build -t $(BINARY):local .

dev:
	cd $(FRONTEND) && npm run dev
