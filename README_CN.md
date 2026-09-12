# http-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/http-kit.svg)](https://pkg.go.dev/github.com/soulteary/http-kit)
[![Go Report Card](.github/goreportcard.svg)](.github/goreportcard-report.md)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![codecov](https://codecov.io/gh/soulteary/http-kit/graph/badge.svg)](https://codecov.io/gh/soulteary/http-kit)

[English](README.md)

一个轻量级的 Go HTTP 客户端库，支持 TLS/mTLS、自动重试（指数退避）以及 OpenTelemetry 链路追踪集成。

## 功能特性

- **TLS/mTLS 支持** - 完整的 TLS 配置，包括 CA 证书、客户端证书用于双向 TLS 认证
- **自动重试** - 可配置的重试逻辑，支持指数退避处理瞬时故障
- **OpenTelemetry 集成** - 内置链路追踪上下文传播，支持分布式追踪
- **灵活配置** - 灵活的客户端配置，提供合理的默认值
- **最小依赖** - 仅依赖 OpenTelemetry 用于追踪（可选）

## 环境要求

- **Go 1.27+**（`go.mod` 声明 `go 1.27.0`）
- 链路上下文传播依赖 `go.opentelemetry.io/otel`

## 安装

```bash
go get github.com/soulteary/http-kit
```

## 快速开始

### 基础 HTTP 客户端

```go
import "github.com/soulteary/http-kit"

// 创建简单客户端
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
    Timeout: 10 * time.Second,
})
if err != nil {
    log.Fatal(err)
}

// 发起请求
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/users", nil)
resp, err := client.Do(req)
```

### 带 TLS/mTLS 的客户端

```go
// 用自定义 CA 校验服务端证书
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://secure-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSServerName: "secure-api.example.com",
})

// 双向 TLS —— 证书和私钥都必须提供
client, err = httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://mtls-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSClientCert: "/path/to/client.crt",
    TLSClientKey:  "/path/to/client.key",
})
```

`NewClient` 会校验这些组合并返回错误，而不是构造出一个悄悄少做了事情的客户端：

- **`Transport` 与 TLS 选项互斥。** 调用方提供的 `RoundTripper` 自带 TLS 配置，
  请直接在该 transport 上配置 TLS。
- **`TLSClientCert` 和 `TLSClientKey` 必须同时设置。** 只给一个会产生一份没有证书
  的 TLS 配置。

使用 TLS 选项时，transport 由 `http.DefaultTransport` 克隆而来，因此代理支持
（`HTTPS_PROXY`）、HTTP/2 和标准连接池上限都会保留。TLS 最低版本为 1.2。

想在构造客户端之前先检查配置，可以自己调用 `opts.Validate()`。

### 自动重试

```go
client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// 默认重试策略：3 次重试，带抖动的指数退避
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/data", nil)
resp, err := client.DoRequestWithRetry(context.Background(), req, nil)

// 也可以自定义
retryOpts := &httpkit.RetryOptions{
    MaxRetries:        5,
    RetryDelay:        200 * time.Millisecond,
    MaxRetryDelay:     5 * time.Second,
    BackoffMultiplier: 2.0,
    RetryableStatusCodes: []int{
        http.StatusTooManyRequests,
        http.StatusServiceUnavailable,
        http.StatusGatewayTimeout,
    },
}
resp, err = client.DoRequestWithRetry(context.Background(), req, retryOpts)
```

#### 什么会被重试

只有同时满足三个条件才会重试：

1. **方法是幂等的，或显式声明可重试。** RFC 9110 定义的幂等方法——`GET`、`HEAD`、
   `PUT`、`DELETE`、`OPTIONS`、`TRACE`，以及空方法（net/http 把它当作 `GET`）——会
   被重试。`POST` 和 `PATCH` 只尝试**一次**，除非请求带上 `Idempotency-Key` header：

   ```go
   req.Header.Set("Idempotency-Key", uuid.NewString())
   ```

2. **请求体可以重放。** 第一次尝试会消费并关闭 `req.Body`，所以重试需要
   `req.GetBody`。`http.NewRequest` 会为常见的内存型 body（`*bytes.Buffer`、
   `*bytes.Reader`、`*strings.Reader`）自动填充它。由任意 `io.Reader` 构造的 body
   没有 `GetBody`，这类请求只尝试一次，而不是带着空 body 重放。

3. **失败是瞬时的。** 可重试的状态码，或另一次尝试有可能解决的传输错误。永久性失败
   不重试：证书校验失败、不支持的 URL scheme、已取消的调用方 context。

#### 退避

延迟为 `RetryDelay × BackoffMultiplier^attempt`，上限为 `MaxRetryDelay`，并带最多
20% 的抖动，这样一批实例在同一次上游故障后不会步调一致地重试。乘数小于 1 表示不增长。

响应的 `Retry-After` header 优先于计算出的延迟——包括 `Retry-After: 0` 和已经过去的
HTTP-date，这两者都表示"立刻重试"。它仍然受 `MaxRetryDelay` 约束（无条件生效，包括
上限为零的配置），超出范围的值会饱和而不是回绕，因此畸形或恶意的上游无法在任一方向上
突破你的上限。

调用方的 context 会附加到请求上，因此取消它会中断正在进行的那次尝试，而不只是中断
两次尝试之间的等待。

被丢弃的响应体会先被读空以便连接回到池里，并同时受字节数、超时和 context 三重约束
——读空永远不会拖住重试。

```go
// 需要时可以直接查看策略
delay := retryOpts.CalculateRetryDelay(2)
retryable := retryOpts.IsRetryableError(err, resp.StatusCode)
retryable = retryOpts.IsRetryableErrorCtx(ctx, err, resp.StatusCode) // 可区分单次
// 尝试的 http.Client.Timeout 与调用方自己的截止时间
```

### OpenTelemetry 链路追踪

```go
import (
    "github.com/soulteary/http-kit"
    "go.opentelemetry.io/otel"
)

client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// 在应用中创建 span
tracer := otel.Tracer("my-service")
ctx, span := tracer.Start(context.Background(), "api-call")
defer span.End()

// 将追踪上下文注入请求头
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/data", nil)
client.InjectTraceContext(ctx, req)

resp, err := client.Do(req)
```

## API 参考

### 客户端选项

| 选项 | 类型 | 默认值 | 描述 |
|------|------|--------|------|
| `BaseURL` | `string` | 必填 | 所有请求的基础 URL |
| `Timeout` | `time.Duration` | `10s` | 请求超时时间 |
| `UserAgent` | `string` | `""` | User-Agent 请求头值 |
| `Transport` | `http.RoundTripper` | `nil` | 自定义 HTTP 传输层 |
| `TLSCACertFile` | `string` | `""` | CA 证书文件路径 |
| `TLSClientCert` | `string` | `""` | 客户端证书文件路径（用于 mTLS） |
| `TLSClientKey` | `string` | `""` | 客户端私钥文件路径（用于 mTLS） |
| `TLSServerName` | `string` | `""` | TLS 验证的服务器名称 |
| `InsecureSkipVerify` | `bool` | `false` | 跳过 TLS 证书验证（不推荐） |

### 重试选项

| 选项 | 类型 | 默认值 | 描述 |
|------|------|--------|------|
| `MaxRetries` | `int` | `3` | 最大重试次数 |
| `RetryDelay` | `time.Duration` | `100ms` | 重试间隔初始延迟 |
| `MaxRetryDelay` | `time.Duration` | `2s` | 重试间隔最大延迟 |
| `BackoffMultiplier` | `float64` | `2.0` | 指数退避乘数 |
| `RetryableStatusCodes` | `[]int` | `[408, 429, 500, 502, 503, 504]` | 触发重试的 HTTP 状态码 |

### 客户端方法

| 方法 | 描述 |
|------|------|
| `NewClient(opts)` | 使用给定选项创建新的 HTTP 客户端 |
| `Do(req)` | 执行 HTTP 请求 |
| `DoRequestWithRetry(ctx, req, retryOpts)` | 执行带自动重试的 HTTP 请求 |
| `InjectTraceContext(ctx, req)` | 将 OpenTelemetry 追踪上下文注入请求头 |
| `GetBaseURL()` | 返回基础 URL |
| `GetHTTPClient()` | 返回底层的 `*http.Client` |

### 辅助函数

| 函数 | 说明 |
|------|------|
| `DefaultOptions()` | 带上表默认值的 `Options` |
| `DefaultRetryOptions()` | 带上表默认值的 `RetryOptions` |
| `(*Options).Validate()` | 不构造客户端也能检查配置 |
| `(*RetryOptions).CalculateRetryDelay(attempt)` | 某次尝试的退避延迟（抖动之前） |
| `(*RetryOptions).IsRetryableError(err, statusCode)` | 该失败是否值得重试 |
| `(*RetryOptions).IsRetryableErrorCtx(ctx, err, statusCode)` | 同上，但能区分单次尝试的超时与调用方的截止时间 |

## 项目结构

```
http-kit/
├── client.go       # HTTP 客户端，支持 TLS/mTLS
├── client_test.go  # 客户端测试
├── retry.go        # 指数退避重试逻辑
├── retry_test.go   # 重试测试
├── go.mod          # 模块定义
└── LICENSE         # Apache 2.0 许可证
```

## 安全特性

| 特性 | 描述 |
|------|------|
| **TLS 校验** | 支持自定义 CA 证书校验服务端 |
| **mTLS 认证** | 支持客户端证书做双向 TLS |
| **服务名校验** | 可配置 TLS 服务名（SNI） |
| **安全默认值** | 默认开启 TLS 校验；TLS 最低版本为 1.2 |
| **不静默降级** | `Transport` 与 TLS 选项同时出现会报错，而不是丢掉 TLS 配置 |
| **重放安全** | `POST`/`PATCH` 只尝试一次，除非带 `Idempotency-Key` |
| **退避有界** | `Retry-After` 不会超过 `MaxRetryDelay`，超范围的值会饱和而不是回绕成负值 |

## 升级说明（v1.5.0）

其中两项会改变"请求到底有没有被发出去"，还有一项可能让原本能用的配置在启动时报错。
没有删除任何 API，新增了一个方法。

- **`Transport` 与任何 TLS 选项同时出现现在会报错。** 设置 `Transport` 此前会静默
  丢弃所有 TLS 选项——构建 TLS 配置的分支根本到不了，于是 mTLS 客户端证书**从未被
  出示，而且没有任何地方报告这件事**。现在 `NewClient` 会返回错误。遇到这个错误时，
  请把 TLS 配置移到你的 transport 上。
- **只设 `TLSClientCert` 不设 `TLSClientKey` 现在会报错。** 它此前会构造出一份不含
  证书的 TLS 配置。
- **重试会重放请求体。** 此前各次尝试复用同一个 `*http.Request`，而第一次 `Do` 会
  消费并关闭 `req.Body`——于是被重试的 `POST` 或 `PUT` 发出的是**空 body**，或者直接
  以 `ContentLength=N with Body length 0` 失败。现在重试会通过 `req.GetBody` 回卷；
  body 无法重放的请求只尝试一次。
- **`POST` 和 `PATCH` 默认不再重试。** 此前会重试，于是一个已经到达服务端的请求被
  重复提交。给非幂等请求加上 `Idempotency-Key` header 即可重新开启重试。**如果你原本
  依赖 `POST` 自动重试，请设置这个 header。**
- **永久性失败不再重试。** 证书校验失败、不支持的 URL scheme、已取消的 context 此前
  都会被重试，而这只是把失败往后推。
- **退避真的是指数的了。** 此前计算的是
  `RetryDelay × (attempt+1) × BackoffMultiplier`，无论乘数是多少都线性增长——乘数
  为 2 时是 200ms、400ms、600ms——尽管字段名不是这么说的。现在是
  `RetryDelay × BackoffMultiplier^attempt`，**并带最多 20% 抖动**。在较大的尝试次数
  上请预期不同（且更大）的延迟，不要断言精确值。
- **`Retry-After` 会被采纳**，并受 `MaxRetryDelay` 约束。显式的 `Retry-After: 0`
  和已经过去的 HTTP-date 都表示"立刻重试"。超范围的值会饱和，而不是回绕成负的
  duration 从而完全绕过上限。
- **context 能中断正在进行的尝试。** `ctx` 参数此前只用于两次尝试之间的等待，从未
  附加到请求上。
- **空 `Method` 按 `GET` 处理。** net/http 对客户端请求就是这么规定的；它此前被归为
  非幂等，于是实际上是 `GET` 的请求遇到 408、429 或 5xx 时不会重试。
- **TLS transport 保留标准默认值。** 它此前是裸的 `&http.Transport{}`：没有
  `Proxy`（`HTTPS_PROXY` 失效）、没有 `ForceAttemptHTTP2`、每主机只有 2 个空闲连接。
  现在由 `http.DefaultTransport` 克隆而来，并设 TLS 1.2 下限。
- **环境要求里写的是 Go 1.26**；`go.mod` 需要 `1.27.0`。

## 测试覆盖率

运行测试并查看覆盖率：

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## 许可证

本项目采用 Apache License 2.0 许可证 - 详见 [LICENSE](LICENSE) 文件。

## 贡献

欢迎贡献！请随时提交 Pull Request。
