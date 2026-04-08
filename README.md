# GateLM

Lightweight AI API gateway with automatic OpenAI/Anthropic protocol translation, multi-backend failover & load balancing, and model alias routing.

轻量级 AI API 网关，支持 OpenAI 与 Anthropic 协议互转、多后端容错负载均衡、模型别名路由，开箱即用。

## Features

- **协议自动转换** — Anthropic ↔ OpenAI 双向转换，含流式 SSE、tool use、thinking/reasoning
- **多后端容错** — 优先级降级 + 加权负载均衡 + 熔断器（healthy/tripped/probing）
- **模型别名路由** — 客户端用 `claude-sonnet-4-6`，网关自动路由到 `gpt-5.4`
- **健康检查** — 被动（请求级）+ 主动（定时探测）双模式
- **WebSocket 代理** — 支持 `/v1/responses` WS 连接，含 WS→SSE 自动降级
- **Token 日志** — SQLite/JSONL 双模式，按客户端 key 记录用量
- **Prometheus 指标** — 请求量、时延、重试、后端尝试，`GET /metrics`
- **Web 管理台** — 配置管理、日志查询、健康监控、在线测试
- **YAML 配置驱动** — 支持热加载，无需重启

## 快速开始

```bash
# 1. 克隆并构建
git clone https://github.com/dotnode/gatelm.git
cd gatelm
go mod tidy

# 2. 创建配置文件
cp config.example.yaml config.yaml
# 编辑 config.yaml，填入后端 URL 和 API Key

# 3. 构建并运行
go build -o gatelm ./cmd/gatelm/
./gatelm config.yaml
```

示例默认监听 `:18765`，启用 debug 日志加 `-debug` 参数。管理台 UI 已内置为单文件 HTML，无需 Node.js 构建。

## 配置说明

完整配置参考 [`config.example.yaml`](config.example.yaml)，以下是核心概念。

### 基础配置

```yaml
listen: ":18765"             # 监听地址
debug: false                 # debug 日志
max_concurrent_requests: 0   # 最大并发，0 = 不限
```

### 后端定义

每个后端独立定义 URL、协议、认证和模型列表：

```yaml
backends:
  - name: my-openai
    url: "https://api.openai.com"
    protocol: "openai"             # openai / anthropic / openai-responses
    api_key: "sk-xxx"              # 按 protocol 自动生成认证头
    models:
      - name: gpt-5.4              # 后端实际模型名
        aliases: [claude-opus-4-6, claude-sonnet-4-6] # 客户端可用的别名
        default_max_tokens: 8192   # 未指定时注入
        default_temperature: 0.6   # 未指定时注入
      - name: gpt-5.4-codex-mini
        aliases: [claude-haiku-4-5-20251001]
```

支持三种后端协议：

| 协议 | 端点 | 说明 |
|------|------|------|
| `openai` | `/v1/chat/completions` | 自动生成 `Authorization: Bearer <api_key>` |
| `openai-responses` | `/v1/responses` | 自动生成 `Authorization: Bearer <api_key>` |
| `anthropic` | `/v1/messages` | 自动生成 `x-api-key`，并补 `anthropic-version` |

### 多后端容错

同一个别名配置在多个后端，按优先级自动降级：

```yaml
backends:
  - name: primary
    url: "https://primary-api.example.com"
    protocol: "openai"
    priority: 1          # 数字越小优先级越高
    weight: 3            # 同优先级内的权重
    timeout: "90s"       # 单后端超时
    health_check:
      path: "/healthz"
      interval: "10s"
    api_key: "sk-primary"
    models:
      - name: gpt-5.4
        aliases: [claude-opus-4-6, claude-sonnet-4-6]

  - name: secondary
    url: "https://secondary-api.example.com"
    protocol: "openai"
    priority: 2          # primary 不可用时自动降级到这里
    api_key: "sk-secondary"
    models:
      - name: gpt-5.4
        aliases: [claude-opus-4-6, claude-sonnet-4-6]   # 同一 alias
```

### 熔断器

```yaml
circuit_breaker:
  fail_threshold: 3          # 连续失败 N 次触发熔断
  recovery_timeout: "30s"    # 冷却窗口
  half_open_max_requests: 1  # 探测并发数
```

三态状态机：`healthy` → 连续失败 → `tripped`（拒绝流量） → 冷却结束 → `probing`（有限探测） → 成功 → `healthy`

### 模型默认参数

按后端模型名集中配置，支持 `模型名@协议` 精确覆盖：

```yaml
model_defaults:
  gpt-5.4:
    reasoning_effort: high       # low / medium / high / xhigh
    max_tokens: 16384
    default_temperature: 0.6
    system_prompt: "你是工程助手。"

  # 协议维度覆盖
  gpt-5.4@anthropic:
    system_prompt: "Anthropic 客户端专用提示词"
  gpt-5.4@openai:
    system_prompt: "none"        # OpenAI 客户端不注入
```

### Token 日志

```yaml
token_log:
  enabled: true
  file: "token_usage.db"     # .db → SQLite, .jsonl → JSONL
  retention_days: 90          # 自动清理，0 = 不清理
```

### 客户端 Key 映射

```yaml
api_keys:
  sk-client-key-1: "alice"   # 日志中显示为 alice
  sk-client-key-2: "bob"     # 未映射的 key 显示掩码+哈希
```

### 管理台

```yaml
console:
  enabled: true
  password: "${CONSOLE_PASSWORD}"  # 支持环境变量
```

默认访问路径为 `http://your-host:port/console`。嵌入到其他服务时可通过 `console.Options{BasePath: ...}` 改成任意子路径。

## 协议自动检测与转换

GateLM 根据请求路径自动识别客户端协议：

| 请求路径 | 检测为 |
|----------|--------|
| `/v1/chat/completions` | OpenAI |
| `/v1/responses` | OpenAI Responses |
| `/v1/messages` | Anthropic |

当客户端协议与后端协议不同时，自动完成双向转换：
- **请求方向**：Anthropic `messages` → OpenAI `chat/completions` 或 `responses`
- **响应方向**：OpenAI 响应 → Anthropic 格式（含流式 SSE 逐块转换）
- **Thinking 转换**：OpenAI `reasoning_content` ↔ Anthropic `thinking` block
- **Tool Use 转换**：完整支持工具调用的双向转换

## API 端点

| 端点 | 说明 |
|------|------|
| `/v1/chat/completions` | OpenAI Chat API 代理 |
| `/v1/responses` | OpenAI Responses API 代理（HTTP + WebSocket） |
| `/v1/messages` | Anthropic Messages API 代理 |
| `/healthz` | 健康检查（始终 200） |
| `/healthz/detail` | 后端详情（全部熔断时 503） |
| `/metrics` | Prometheus 指标 |
| `/console` | Web 管理台 |

## 嵌入到其他 Go 项目

除独立二进制外，也支持作为 Go 包嵌入。当前推荐把 `pkg/gatelm` 作为代理运行时，按需再挂载 `pkg/console`：

```go
package main

import (
  "log"
  "net/http"

  "github.com/dotnode/gatelm/pkg/console"
  "github.com/dotnode/gatelm/pkg/gatelm"
)

func main() {
  cfg, err := gatelm.LoadConfig("config.yaml")
  if err != nil {
    log.Fatal(err)
  }

  gateway, err := gatelm.New(gatelm.Options{
    Config:     cfg,
    ConfigPath: "config.yaml",
  })
  if err != nil {
    log.Fatal(err)
  }
  defer gateway.Close()

  mux := http.NewServeMux()
  console.Mount(mux, gateway, console.Options{BasePath: "/admin/ai"})
  mux.Handle("/", gateway.Handler())

  log.Fatal(http.ListenAndServe(":8080", mux))
}
```

- `pkg/gatelm`：公开代理 SDK，包含配置加载、运行时初始化、热重载、`/healthz`、`/healthz/detail`、`/metrics`
- `gatelm.Config` / `gatelm.Backend` / `gatelm.Model`：公开类型别名，宿主项目可直接组装配置
- `pkg/console`：可选管理台挂载层，支持自定义 `BasePath`
- `gatelm.ValidateConfig(cfg)`：宿主项目自行构造配置时，可复用与 YAML 加载一致的归一化和校验逻辑
- 默认管理台路径仍为 `/console`；嵌入时可改成 `/admin/ai` 等任意子路径

如果只需要代理能力，不需要管理台，也可以只挂主处理器：

```go
mux := http.NewServeMux()
mux.Handle("/", gateway.Handler())
```

## 架构说明

- `cmd/gatelm` 现在是组装层：读取配置、初始化 `Gateway`、挂载 console、启动 HTTP server
- `pkg/gatelm` 封装核心代理运行时，对外暴露嵌入式 SDK
- `pkg/console` 提供可选管理台挂载层，适合宿主项目按路径接入
- Console UI 为 `internal/server/consoleui/index.html` 单文件页面，通过 `go:embed` 内置，并在服务端注入 base path

## 生产部署

```bash
# 构建
go build -o gatelm ./cmd/gatelm/

# 直接运行
./gatelm config.yaml

# 或使用 systemd（参考 config 中的 ExecStart）
# 一键构建+重启脚本
./restart.sh
```

## GitHub Release

仓库新增了自动版本发布工作流 [`release.yml`](.github/workflows/release.yml)：

- 推送 tag（如 `v1.0.0`）后会自动创建/更新 GitHub Release
- 也支持在 Actions 页面手动触发，并指定一个已存在的 tag
- Release 会产出 `linux`、`windows`、`darwin` 的压缩包，覆盖 `amd64` 和 `arm64`
- 每个压缩包内固定只有两个文件：`gatelm`/`gatelm.exe` 和 `config.yaml`

压缩包命名格式：

```text
gatelm_<version>_<goos>_<goarch>.zip
```

例如：

```text
gatelm_v1.0.0_linux_amd64.zip
gatelm_v1.0.0_windows_amd64.zip
gatelm_v1.0.0_darwin_arm64.zip
```

### 生产特性

- **Graceful shutdown** — SIGINT/SIGTERM 后等待请求完成（15s 超时）
- **HTTP 连接池** — dial 30s、TLS 10s、idle 90s、最大空闲连接 100
- **请求体限制** — 32MB，超限返回 413
- **Request ID** — 每请求生成 `X-Request-Id`
- **WebSocket** — 支持 WS 代理，含 WS→SSE 自动降级（`sse_only` 模式）

## 项目结构

```
cmd/gatelm/main.go           # 组装层：加载配置、初始化 Gateway、挂载 console、启动服务
pkg/
  gatelm/
    gateway.go               # 公开 Gateway SDK：Handler/Reload/Close
    config.go                # 公开配置类型、LoadConfig、ValidateConfig
  console/
    console.go               # 可选管理台挂载入口（支持自定义 BasePath）
internal/
  config/config.go            # 配置结构体、YAML 加载、校验
  logging/
    debug.go                  # debug 日志（按日期滚动）
    token.go                  # Token 日志（SQLite/JSONL）
  server/
    server.go                 # 请求主逻辑、forwardWithRetry
    codec.go                  # ProtocolCodec 接口、流式管线
    codec_anthropic.go        # Anthropic codec
    codec_openai.go           # OpenAI Chat codec
    codec_responses.go        # OpenAI Responses codec
    convert.go                # 协议转换函数
    health.go                 # 健康检查管理器
    selector.go               # 后端选择器（优先级+加权轮询）
    resolve.go                # 模型别名索引
    metrics.go                # Prometheus 指标
    console.go                # 管理台 API
    console_embed.go          # 管理台静态资源与 base path 注入
    websocket.go              # WebSocket 代理
    consoleui/index.html      # 内置单文件管理台 UI（Vue 3 + Tailwind CDN）
config.example.yaml           # 配置示例
```

## License

[MIT](LICENSE)
