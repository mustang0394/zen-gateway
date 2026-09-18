# zen-gateway

[![Release](https://img.shields.io/github/v/release/OWNER/REPO?sort=semver&label=release)](https://github.com/OWNER/REPO/releases)
[![Docker Image](https://img.shields.io/badge/ghcr.io-zen--gateway-blue)](https://github.com/OWNER/REPO/pkgs/container/zen-gateway)
![Go](https://img.shields.io/badge/Go-1.27+-00ADD8?logo=go&logoColor=white)
![Platforms](https://img.shields.io/badge/platforms-linux%20%7C%20windows-informational)

一个轻量的 **OpenAI 兼容中转网关**，让任意 OpenAI SDK / 客户端无需任何特殊配置，即可调用
[OpenCode Zen](https://opencode.ai/zen) 提供的免费模型。

```
OpenAI 客户端 ──► zen-gateway ──► opencode.ai/zen/v1
                (自动补齐身份要素)
```

- **零改动接入**：客户端只需指向网关地址，UA / session / tools / stream 全部由网关代为处理
- **多 key 多出口**：按 API key 路由到不同的 HTTP / SOCKS5 代理，未配置的 key 自动直连
- **开箱即用**：单个静态二进制，官方 Docker 镜像（amd64 / arm64），内置健康检查
- **自动发版**：GitHub Actions 自动叠加版本号，发布二进制 Release 与 GHCR 镜像

> [!IMPORTANT]
> 本项目为社区逆向研究成果，仅供学习研究使用。免费层服务由 OpenCode 官方提供，
> 请遵守其[服务条款](https://opencode.ai/zen)，勿滥用。

## 目录

- [快速开始](#快速开始)
- [工作原理](#工作原理)
- [配置](#配置)
- [部署](#部署)
- [模型列表](#模型列表)
- [构建与发布](#构建与发布)
- [常见问题](#常见问题)

## 快速开始

### Docker（推荐）

```bash
docker run -d --name zen-gateway -p 8080:8080 ghcr.io/mustang0394/zen-gateway:latest
```

验证：

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "mimo-v2.5-free", "messages": [{"role":"user","content":[{"type":"text","text":"你好"}]}]}'
```

### 二进制

从 [Releases](https://github.com/OWNER/REPO/releases) 下载对应平台二进制（含 sha256 校验文件）：

```bash
chmod +x zen-gateway-linux-amd64 && ./zen-gateway-linux-amd64 -listen :8080
```

### 接入 OpenAI SDK

任何支持自定义 `baseURL` 的 OpenAI 客户端均可直接使用，无需特殊请求头：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/v1",
    api_key="public",  # 免费层使用 public；填入自己的 Zen key 则按 key 路由
)
resp = client.chat.completions.create(
    model="mimo-v2.5-free",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

## 工作原理

Zen 的免费模型上游会校验请求是否来自 opencode 官方客户端，不满足条件的请求会被拒绝
（`FreeTierError: OpenCode's free tier can only be used from within OpenCode`）。

zen-gateway 在网关层透明地补齐全部要素，客户端无需感知：

| 上游校验项 | 网关处理 |
|---|---|
| `User-Agent: opencode/<semver>` | 每日从 GitHub Releases 同步最新版本号，无条件覆盖客户端值 |
| `x-opencode-session`（opencode 会话 ID 格式） | 客户端未传时按 opencode ID 算法生成；已传则保留 |
| 请求 `tools` 中同时存在 `bash` 与 `read` 工具 | 缺失的工具以最小 schema 注入；已有的原样保留 |
| `stream: true` | 强制改写为流式，并向上游原样转发 SSE 流 |

此外，网关默认执行安全转发策略：

- **请求头白名单**：仅转发 `Accept`、`Authorization`、`Content-Type`、`User-Agent`
  与 `x-opencode-*` 四个头，客户端 SDK 注入的其他头（Cookie、`x-stainless-*` 等）一律丢弃
- **默认值补齐**：`Authorization` 缺失补 `Bearer public`；`x-opencode-client` 缺失补
  `cli`；`x-opencode-project` 缺失补 `global`；`x-opencode-request` 缺失自动生成
- **无损转码**：请求体经 `json.Decoder.UseNumber` 处理，重编码不丢失数字精度

## 配置

所有配置项均有默认值，最小化即可运行。来源优先级：**命令行 flag > 环境变量 > 配置文件 > 默认值**。

### 监听地址

```bash
./zen-gateway -listen :9000        # flag
ZEN_LISTEN=:9000 ./zen-gateway     # 环境变量
```

### key → 代理路由

按客户端传入的 API key 将请求路由到不同的网络出口，适用于多账号分散限流或地域解锁场景。

环境变量方式（推荐，Docker 友好）：

```bash
export ZEN_PROXIES='{
  "public":      "http://proxy-a.example.com:8080",
  "oc_sk_key_1": "socks5h://user:pass@proxy-b.example.com:1080",
  "oc_sk_key_2": "socks5://proxy-c.example.com:1080"
}'
./zen-gateway
```

配置文件方式：

```json
{
  "listen": ":8080",
  "proxies": {
    "public": "http://proxy-a.example.com:8080",
    "oc_sk_key_1": "socks5h://user:pass@proxy-b.example.com:1080"
  }
}
```

```bash
./zen-gateway -config /etc/zen-gateway.json
```

路由规则：

- 客户端 `Authorization: Bearer <key>` 命中 `proxies` 中的键 → 该请求走对应代理
- 未命中（含未传 key 时的默认 `public` 无映射）→ 直连
- 代理协议支持 `http://`、`https://`、`socks5://`、`socks5h://`（`socks5h` 表示域名解析在代理端完成）
- 代理客户端按需创建并复用

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `:8080` | 监听地址（覆盖 env / 配置文件） |
| `-config` | — | JSON 配置文件路径 |
| `-upstream` | `https://opencode.ai/zen/v1` | 上游基址 |
| `-verbose` | off | 调试日志 |

## 端点

| 网关路径 | 上游路径 | 适用模型 |
|---|---|---|
| `POST /v1/chat/completions` | `/chat/completions` | chat 风格模型（见[模型列表](#模型列表)） |
| `POST /v1/responses` | `/responses` | responses 风格模型 |
| `GET /healthz` | — | 健康检查，返回 `ok` |

`/v1` 前缀可省略。

## 部署

### Docker Run

```bash
# 最简部署（全直连）
docker run -d --name zen-gateway -p 8080:8080 ghcr.io/mustang0394/zen-gateway:latest

# 带代理路由
docker run -d --name zen-gateway -p 8080:8080 \
  -e ZEN_PROXIES='{"public":"http://10.0.0.5:8080","oc_sk_x1":"socks5h://user:pass@proxy:1080"}' \
  ghcr.io/mustang0394/zen-gateway:latest
```

### Docker Compose

```yaml
services:
  zen-gateway:
    image: ghcr.io/mustang0394/zen-gateway:latest
    ports:
      - "8080:8080"
    environment:
      ZEN_LISTEN: ":8080"
      ZEN_PROXIES: '{"public":"socks5h://user:pass@proxy:1080"}'
    restart: unless-stopped
```

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZEN_LISTEN` | `:8080` | 监听地址 |
| `ZEN_PROXIES` | 空（全直连） | key→代理 JSON 对象 |

镜像基于 Alpine，多阶段构建，非 root 用户运行，内置健康检查，无需挂载任何文件。

## 模型列表

完整列表可通过上游接口查询：

```bash
curl https://opencode.ai/zen/v1/models
```

当前可用的免费模型（经网关验证可用）：

| 模型 ID | 端点 |
|---|---|
| `mimo-v2.5-free` | `/chat/completions` |
| `deepseek-v4-flash-free` | `/chat/completions` |
| `nemotron-3-ultra-free` | `/chat/completions` |
| `nemotron-3.5-lightning-free` | `/chat/completions` |
| `ling-3.0-flash-fin-free` | `/chat/completions` |
| `muse-spark-1.3-contributor-free` | `/responses` |
| `muse-spark-1.2-contributor-free` | `/responses` |

> [!NOTE]
> 模型以 `/responses` 或 `/chat/completions` 端点区分，混用会得到上游错误。
> 免费层按 IP 限流；持有付费 Zen API key 的请求不受免费层限制。

## 构建与发布

### 本地构建

```bash
go build -o zen-gateway ./cmd/gateway
```

交叉编译（静态二进制，无 CGO）：

```bash
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o zen-gateway-linux-arm64 ./cmd/gateway
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o zen-gateway.exe ./cmd/gateway
```

本地构建镜像：

```bash
docker build -t zen-gateway .
```

### 自动发布（GitHub Actions）

工作流 [`.github/workflows/release.yml`](.github/workflows/release.yml)
在单次触发中同时产出 **GitHub Release** 与 **GHCR 镜像**：

| 产物 | 内容 |
|---|---|
| GitHub Release | `linux/amd64`、`linux/arm64`、`windows/amd64`、`windows/arm64` 二进制 + sha256 |
| GHCR 镜像 | `linux/amd64` + `linux/arm64` 多架构镜像 |

两种触发方式：

1. **push tag**（手动定版）：`git tag v1.2.3 && git push origin v1.2.3`，直接以该 tag 发布
2. **workflow_dispatch**（自动叠版本）：取仓库最新 tag，按所选分量（`major`/`minor`/`patch`，默认
   `patch`）+1 后自动打 tag 并发布。例如最新 `v1.18.31` → patch 触发后发布 `v1.18.32`；
   若目标 tag 已存在则继续 +1 跳过，避免冲突

镜像标签随版本自动生成：`1.18.32`（精确）、`1.18`（主次）、`latest`（正式 release 置顶）。

发布物为**正式 release**（非 pre-release），编译失败不会产生空 release。

## 项目结构

```
zen-gateway/
├── cmd/gateway/           # 入口：flag 解析、路由注册、优雅退出
├── internal/
│   ├── config/            # 配置合并（flag > env > file > default）
│   ├── idgen/             # opencode 格式 ID 生成（ses_/msg_）
│   ├── version/           # GitHub 版本同步与 UA 维护
│   ├── upstream/          # 代理客户端构建（http/socks5）
│   └── ztproxy/           # 核心转发：header 过滤、body 改写、SSE 透传
├── Dockerfile             # 多阶段构建（golang → alpine）
└── .github/workflows/     # 自动发布工作流
```

## 常见问题

**为什么上游返回 `FreeTierError`？**

网关已自动补齐全部身份要素，出现该错误通常意味着上游调整了校验规则。
请更新到最新版本，并在 [Issues](https://github.com/OWNER/REPO/issues) 反馈。

**免费额度是多少？**

由上游按 IP 限流，具体数值未公开。出现 429 时请降低请求频率或更换出口 IP
（通过 [key→代理路由](#key--代理路由)配置）。

**支持哪些上游协议？**

兼容 OpenAI Chat Completions 与 Responses 两套 API。网关本身不做协议转换，
请求格式需与所选模型的端点匹配。

**会记录我的请求内容吗？**

网关自身无存储、无日志落盘（仅标准输出运行日志，不含请求体内容）；请求仅经内存转发。

## 许可

仅供学习研究使用。使用本项目即表示你同意遵守上游服务的使用条款，因滥用导致的账号限制
或法律风险由使用者自行承担。
