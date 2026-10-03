# zen-gateway

一个轻量的 **OpenAI 兼容中转网关**，把两类上游统一成一套 OpenAI 风格接口，并内置
**Key 池管理、顺序轮询、冷却池与用量统计**，配套 Web 管理端。

```
OpenAI 客户端 ──┬──► /zen/v1/*   ──► zen 模块   ──► opencode.ai/zen/v1
                └──► /cline/v1/* ──► cline 模块 ──► api.cline.bot/api/v1
                                          │
                          浏览器 ──► /admin（Web 管理端）
```

- **多上游**：`zen` 与 `cline` 两个模块各自独立实现，互不影响
- **Key 池**：上游 Key 由网关维护，下游只用一枚接入 Token；每个 Key 可单独指定代理
- **顺序轮询**：始终优先使用排在前面的 Key，冷却后再切换，尽量保持上游缓存命中率
- **冷却池**：按「Key + 模型」维度冷却，某 Key 的某模型冷却不影响该 Key 的其他模型
- **用量统计**：总 / 输入 / 输出 / 缓存命中 token、缓存命中率、平均首字用时（TTFT）
- **零配置文件**：所有业务配置存在 SQLite 中，由 Web 管理端维护
- **单二进制**：前端资源内嵌，纯 Go（无 cgo），支持 linux/windows × amd64/arm64

> [!IMPORTANT]
> 本项目为社区逆向研究成果，仅供学习研究使用。上游服务由官方提供，
> 请遵守各上游的[服务条款](https://opencode.ai/zen)，勿滥用。

## 目录

- [快速开始](#快速开始)
- [端点](#端点)
- [Web 管理端](#web-管理端)
- [工作原理](#工作原理)
- [冷却与轮询](#冷却与轮询)
- [统计](#统计)
- [配置](#配置)
- [部署](#部署)
- [构建](#构建)
- [项目结构](#项目结构)

## 快速开始

### Docker（推荐）

```bash
docker run -d --name zen-gateway -p 8080:8080 \
  -e ZEN_ADMIN_TOKEN=change-me \
  -v zen-data:/data \
  ghcr.io/mustang0394/zen-gateway:latest
```

### 二进制

从 [Releases](https://github.com/mustang0394/zen-gateway/releases) 下载对应平台二进制：

```bash
chmod +x zen-gateway-linux-amd64
ZEN_ADMIN_TOKEN=change-me ./zen-gateway-linux-amd64 -listen :8080
```

### 首次配置

1. 打开管理端 `http://127.0.0.1:8080/admin`，用 `ZEN_ADMIN_TOKEN` 登录
2. 在 **Key 管理** 中为 `zen` / `cline` 分别添加上游 Key（可指定代理，留空为直连）
3. 在 **设置** 中配置下游接入 Token（`zen` 可留空以允许匿名访问）
4. 调用网关：

```bash
curl http://127.0.0.1:8080/zen/v1/chat/completions \
  -H "Authorization: Bearer <接入Token>" \
  -H "Content-Type: application/json" \
  -d '{"model": "mimo-v2.5-free", "messages": [{"role":"user","content":"你好"}]}'
```

### 接入 OpenAI SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:8080/zen/v1",   # 或 /cline/v1
    api_key="<接入Token>",
)
resp = client.chat.completions.create(
    model="mimo-v2.5-free",
    messages=[{"role": "user", "content": "你好"}],
)
print(resp.choices[0].message.content)
```

## 端点

两个模块的上游基址均**已包含 `/v1`**（`.../zen/v1`、`.../api/v1`），
因此下表「上游路径」一列只是基址之后拼接的业务路径。

| 网关路径 | 上游路径 | 说明 |
|---|---|---|
| `POST /zen/v1/chat/completions` | `/chat/completions` | zen chat 风格模型 |
| `POST /zen/v1/responses` | `/responses` | zen responses 风格模型 |
| `GET  /zen/v1/models` | `/models` | zen 模型列表 |
| `POST /cline/v1/chat/completions` | `/chat/completions` | cline chat 风格模型 |
| `POST /cline/v1/responses` | `/responses` | cline responses 风格模型 |
| `GET  /cline/v1/models` | `/models` | cline 模型列表 |
| `GET  /admin` | — | Web 管理端 |
| `GET  /healthz` | — | 健康检查 |

路径中的 `/v1` 可省略（`/zen/chat/completions` 等价）。

**下游鉴权**：两个模块各有一枚接入 Token（在 Web 「设置」中配置），客户端用
`Authorization: Bearer <token>` 携带。

- `zen`：Token 留空时允许**匿名**访问（上游 Key 填 `public` 即可，上游按 IP 限流）
- `cline`：上游不支持匿名，必须配置有效 Key；建议同时配置接入 Token

## Web 管理端

访问 `/admin`，使用环境变量 `ZEN_ADMIN_TOKEN` 作为口令。**未设置该变量时管理端整体禁用**
（返回 501），不会裸奔。

| 页面 | 功能 |
|---|---|
| 总览 / 统计 | 当日汇总卡片（总 token、输入/输出、缓存命中、命中率、平均首字、请求数）+ 按 Key×模型明细 + 近 30 天日期切换 |
| Key 管理 | Zen / Cline 分栏；新增、编辑、启停、删除、**拖拽式排序**（决定轮询顺序）、备注、代理配置与校验 |
| 版本号 | 展示 `zen` / `cline.cli` / `cline.sdk` 三个版本号、上次抓取时间与结果，支持手动刷新 |
| 冷却池 | 查看当前冷却（模块 / Key / 模型 / 剩余时间 / 原因），支持手动解除与手动添加 |
| 设置 | 下游接入 Token、统计保留天数、上游地址（只读） |

管理端为 React SPA，前端资源在构建时打包进二进制，**运行时不依赖任何外部 CDN**。

## 工作原理

### zen 模块

上游会校验请求是否来自 opencode 官方客户端，不满足即返回
`FreeTierError: OpenCode's free tier can only be used from within OpenCode`。
网关透明补齐全部要素：

| 上游校验项 | 网关处理 |
|---|---|
| `User-Agent: opencode/<semver>` | 每日从 GitHub 同步最新版本号，无条件覆盖客户端值 |
| `x-opencode-session` | 缺失时按 opencode ID 算法生成（`ses_` + 12 hex + 14 base62） |
| `x-opencode-request` | 缺失时生成 `msg_` ID |
| `x-opencode-client` / `x-opencode-project` | 缺失时补 `cli` / `global` |
| `Authorization` | 失败时补 `Bearer public`；Key 由网关侧从池中选择 |
| 请求体 `tools` 含 `bash` 与 `read` | 缺失的以最小 schema 注入（chat 与 responses 两种结构分别处理） |
| `stream: true` | 强制改写为流式，并原样转发上游 SSE |

另外执行安全转发策略：**请求头白名单**（仅 `Accept`、`Authorization`、`Content-Type`、
`User-Agent` 与 `x-opencode-*` 透传，Cookie、`x-stainless-*` 等一律丢弃）；请求体经
`json.Decoder.UseNumber` 处理，重编码不丢失数字精度。

### cline 模块

上游依据请求头判定客户端身份，因此网关**完全忽略下游传入的请求头**，
只发送一组固定的 Cline 产品头：

| Header | 取值 |
|---|---|
| `Authorization` | `Bearer <池中 Key>` |
| `HTTP-Referer` | `https://cline.bot` |
| `X-Title` | `Cline` |
| `X-IS-MULTIROOT` | `false` |
| `X-CLIENT-VERSION` | 每日抓取的主版本，如 `4.1.22` |
| `X-PLATFORM` | `cli` |
| `X-PLATFORM-VERSION` | 同 `X-CLIENT-VERSION` |
| `X-CORE-VERSION` | 每日抓取的 SDK 版本，如 `0.0.90` |
| `User-Agent` | `Cline/<主版本>` |
| `X-CLIENT-TYPE` | `cline-cli` |

**403 重试**：当上游返回 403 且错误信息包含
`is only available via Cline product surfaces` 时，网关对**同一个 Key** 固定重试
3 次（退避 300ms / 800ms / 1500ms）；3 次后仍是该错误则原样返回下游。

### 版本号来源

| 目标 | 来源 | 用途 |
|---|---|---|
| `zen` | `anomalyco/opencode` 最新正式 release（`v1.18.34` → `1.18.34`） | `User-Agent: opencode/<v>` |
| `cline.cli` | `cline/cline` 发布列表中最大的 `v*`（排除 `cli-v*`、`desktop-v*`、`sdk/*`、预发布） | `X-CLIENT-VERSION`、`X-PLATFORM-VERSION`、`User-Agent` |
| `cline.sdk` | 同上列表中最大的 `sdk/sdk/v*` | `X-CORE-VERSION` |

启动时立即抓取一次，之后每 24 小时刷新；抓取失败会保留上一次的值（首次失败使用内置兜底值），
结果与失败原因均可在管理端查看。

## 冷却与轮询

### 轮询规则

1. 按 `sort_order` 升序挑选**第一个可用 Key**（管理端拖动排序）
2. 顺序优先（而非轮询）可让同一模型持续命中同一 Key，从而保持上游 prompt 缓存命中率
3. 该 Key 冷却或不可用时，自动切换到下一个 Key
4. 全部 Key 均不可用时返回 `429`，并在错误体中给出模型信息

### 冷却规则（维度：模块 + Key + 模型）

| 模块 | 触发条件 | 冷却时长 |
|---|---|---|
| `zen` | 仅依据上游返回 `429` | 到**次日 0 点**（免费额度按天重置） |
| `cline` | 上游返回 `429` | 解析错误文本中的时长，如 `Try again in 22h 59m`；解析失败时兜底 1 小时 |

冷却粒度是「Key + 模型」，因此某 Key 在模型 A 上冷却后，它仍可继续服务模型 B。
所有冷却记录可在管理端查看、手动解除或手动添加。

## 统计

转发响应时同步解析用量，不额外请求上游：

| 指标 | 来源 |
|---|---|
| 输入 token | `usage.prompt_tokens`（responses：`input_tokens`） |
| 输出 token | `usage.completion_tokens`（responses：`output_tokens`） |
| 缓存命中 token | `prompt_tokens_details.cached_tokens`（responses：`input_tokens_details.cached_tokens`） |
| 总 token | `usage.total_tokens`，缺失时取输入 + 输出 |
| 缓存命中率 | 缓存命中 / 输入（聚合时计算） |
| 平均首字用时 | 首个含文本增量的 SSE chunk 与请求发出的时间差 |

统计维度为 `日期 × 模块 × Key × 模型`，默认保留 30 天（可配置），过期数据由后台任务
每天自动清理。流式统计依赖上游返回 usage；若上游未返回，则降级为只记录请求数与 TTFT，
不影响转发本身。

## 配置

配置来源优先级：**命令行 flag > 环境变量 > 默认值**。
上游 Key、代理、接入 Token、冷却参数等业务配置存在 SQLite 中，由 Web 管理端维护。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZEN_LISTEN` | `:8080` | 监听地址 |
| `ZEN_ADMIN_TOKEN` | — | Web 管理端口令；**未设置则禁用管理端** |
| `ZEN_DATA_DIR` | `data` | 数据目录（存放 SQLite） |
| `ZEN_DB_PATH` | `<data-dir>/gateway.db` | SQLite 文件路径（优先于 `ZEN_DATA_DIR`） |
| `ZEN_UPSTREAM` | `https://opencode.ai/zen/v1` | zen 上游基址 |
| `ZEN_CLINE_UPSTREAM` | `https://api.cline.bot/api/v1` | cline 上游基址（含 `/v1`，与 zen 对称） |
| `ZEN_RETENTION_DAYS` | `30` | 统计与请求明细保留天数 |
| `ZEN_CLINE_COOLDOWN_FALLBACK` | `1h` | cline 429 无法解析时长时的兜底冷却 |

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `:8080` | 监听地址（覆盖 `ZEN_LISTEN`） |
| `-data-dir` | `data` | 数据目录 |
| `-db` | — | SQLite 路径（覆盖 `-data-dir`） |
| `-upstream-zen` | 见上 | zen 上游基址 |
| `-upstream-cline` | 见上 | cline 上游基址（含 `/v1`） |
| `-retention-days` | `30` | 统计保留天数 |
| `-verbose` | off | 调试日志 |

### 代理

每个上游 Key 可单独配置网络出口，适用于多账号分散限流或地域解锁：

- `http://host:port`、`https://host:port` — CONNECT 隧道
- `socks5h://user:pass@host:port` — 域名由代理端解析（远端解析）
- `socks5://user:pass@host:port` — 网关本地解析域名后再交给代理

留空即直连。Key 与代理的对应关系在管理端维护。

## 部署

### Docker Compose

```yaml
services:
  zen-gateway:
    image: ghcr.io/mustang0394/zen-gateway:latest
    ports:
      - "8080:8080"
    environment:
      ZEN_ADMIN_TOKEN: change-me          # 管理端口令
      ZEN_RETENTION_DAYS: "30"
      TZ: Asia/Shanghai                   # 影响 zen 的"次日 0 点"冷却
    volumes:
      - zen-data:/data                    # SQLite 数据库与统计数据
    restart: unless-stopped

volumes:
  zen-data:
```

镜像基于 Alpine，多阶段构建，非 root 用户运行，内置健康检查。
上游 Key 与设置全部存在 `/data` 卷中，升级镜像不会丢失。

### 数据目录权限（`/data`）

容器内以非 root 的 `zen` 用户运行，其 **UID/GID 固定为 `10001:10001`**
（可在构建时用 `--build-arg PUID=... PGID=...` 覆盖，需同时保证 `/data` 属主一致）。

**命名卷（推荐，无需任何 chown）**

Docker 首次创建命名卷时会把镜像中 `/data` 的属主与权限一并复制过去，
而镜像里 `/data` 已属于 `zen`，因此容器可直接读写：

```yaml
volumes:
  - zen-data:/data
```

**bind mount（需手动 chown）**

bind mount 不会复制镜像内的属主，会沿用宿主机目录的 owner，因此首次部署需：

```bash
mkdir -p ./data
sudo chown -R 10001:10001 ./data      # 与镜像内 zen 用户一致
docker run -d --name zen-gateway -p 8080:8080 \
  -e ZEN_ADMIN_TOKEN=change-me \
  -v "$PWD/data:/data" \
  ghcr.io/mustang0394/zen-gateway:latest
```

若省略这一步，容器会因无法写入 SQLite 而启动失败，日志中会出现
`open store: ... permission denied`。

> 权限说明：数据库文件以 `0700` 目录权限创建，仅 `zen` 用户可读写；
> 若你需要用宿主机工具直接查看 `gateway.db`，可自行调整目录权限。

## 构建

### 本地构建

前端资源通过 `go:embed` 内嵌。仓库已提交构建产物，因此 `go build` 可独立工作；
修改前端后需要重新构建：

```bash
make web      # 构建前端 -> internal/web/dist
make build    # 编译二进制（含前端）
make test     # 全部测试（含 -race）
make dev      # 前端开发服务器（代理到本地 8080）
```

仅编译 Go 部分（跳过前端）：

```bash
make build-go
# 等价于
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o zen-gateway ./cmd/gateway
```

### 交叉编译

SQLite 使用纯 Go 驱动（`modernc.org/sqlite`），无需 cgo：

```bash
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -o zen-gateway-linux-arm64 ./cmd/gateway
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o zen-gateway.exe ./cmd/gateway
```

### 自动发布（GitHub Actions）

工作流 [`.github/workflows/release.yml`](.github/workflows/release.yml) 在单次触发中同时产出
**GitHub Release** 与 **GHCR 镜像**：

| 产物 | 内容 |
|---|---|
| GitHub Release | `linux/amd64`、`linux/arm64`、`windows/amd64`、`windows/arm64` 二进制 + sha256 |
| GHCR 镜像 | `linux/amd64` + `linux/arm64` 多架构镜像 |

两种触发方式：

1. **push tag**（手动定版）：`git tag v1.2.3 && git push origin v1.2.3`
2. **workflow_dispatch**（自动叠版本）：取最新 tag 按所选分量（`major`/`minor`/`patch`，默认
   `patch`）+1 后自动打 tag 并发布；目标 tag 已存在则继续 +1 跳过

## 项目结构

```
zen-gateway/
├── cmd/gateway/           # 入口：flag/env 解析、装配、优雅退出
├── internal/
│   ├── config/            # 进程级配置（flag > env > 默认）
│   ├── store/             # SQLite 持久化：迁移、DAO、统计批量落盘
│   ├── provider/          # 上游模块抽象与公共转发骨架
│   │   ├── zen/           # zen：头改写、tools 注入、stream 强制、429→次日 0 点
│   │   └── cline/         # cline：固定头、403 重试、429 时长解析
│   ├── cooldown/          # 冷却池服务与数据保留策略
│   ├── stats/             # SSE 解析：usage 与 TTFT 采集
│   ├── version/           # 多目标版本管理（zen / cline.cli / cline.sdk）
│   ├── router/            # /zen、/cline、/admin、/healthz 分发
│   ├── web/               # 管理端 REST API + 内嵌前端资源
│   ├── upstream/          # 按代理构建 HTTP 客户端（http/socks5/socks5h）
│   └── idgen/             # opencode 格式 ID 生成
├── frontend/              # React + TypeScript + Tailwind 管理端源码
│   └── src/{pages,components/ui,lib}/
├── scripts/build-web.sh   # 前端构建脚本
├── Dockerfile             # node → golang → alpine 三阶段构建
└── Makefile
```

## 常见问题

**为什么 zen 返回 `FreeTierError`？**

网关已自动补齐全部身份要素，出现该错误通常意味着上游调整了校验规则。
请更新到最新版本，并在 [Issues](https://github.com/mustang0394/zen-gateway/issues) 反馈。

**为什么 cline 连续 403？**

cline 上游会校验客户端版本。网关已按最新版本号构造请求头，且对
`is only available via Cline product surfaces` 类错误固定重试 3 次。
若持续失败，请在管理端「版本号」页手动刷新后重试，或确认所用 Key 有效。

**出现 429 / 全部 Key 都在冷却？**

免费层按天/按 IP 限流。可添加更多 Key（并配置不同代理出口），
或在管理端「冷却池」中查看剩余时间与原因、必要时手动解除。

**支持哪些上游协议？**

兼容 OpenAI Chat Completions 与 Responses 两套 API。网关本身不做协议转换，
请求格式需与所选模型的端点匹配（`chat/completions` 与 `responses` 不可混用）。

**会记录我的请求内容吗？**

不会。网关只记录请求的元数据（模块、Key、模型、状态码、耗时、token 用量），
**不记录请求体与响应体内容**。统计数据存在本地 SQLite，默认保留 30 天。

## 许可

仅供学习研究使用。使用本项目即表示你同意遵守各上游服务的使用条款，
因滥用导致的账号限制或法律风险由使用者自行承担。
