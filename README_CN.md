# http-kit

[![Go Reference](https://pkg.go.dev/badge/github.com/soulteary/http-kit.svg)](https://pkg.go.dev/github.com/soulteary/http-kit)
[![Go Report Card](https://goreportcard.com/badge/github.com/soulteary/http-kit)](https://goreportcard.com/report/github.com/soulteary/http-kit)
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
// 带服务器证书验证的客户端
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://secure-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSServerName: "secure-api.example.com",
})

// 双向 TLS（mTLS）客户端
client, err := httpkit.NewClient(&httpkit.Options{
    BaseURL:       "https://mtls-api.example.com",
    TLSCACertFile: "/path/to/ca.crt",
    TLSClientCert: "/path/to/client.crt",
    TLSClientKey:  "/path/to/client.key",
})
```

### 自动重试

```go
import "github.com/soulteary/http-kit"

client, _ := httpkit.NewClient(&httpkit.Options{
    BaseURL: "https://api.example.com",
})

// 使用默认重试选项（3 次重试，指数退避）
req, _ := http.NewRequest("GET", client.GetBaseURL()+"/data", nil)
resp, err := client.DoRequestWithRetry(context.Background(), req, nil)

// 或自定义重试行为
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
resp, err := client.DoRequestWithRetry(context.Background(), req, retryOpts)
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
| **TLS 验证** | 支持自定义 CA 证书进行服务器验证 |
| **mTLS 认证** | 支持客户端证书进行双向 TLS |
| **服务器名称验证** | 可配置 TLS 服务器名称用于 SNI |
| **安全默认值** | 默认启用 TLS 验证 |

## 测试覆盖率

运行测试并查看覆盖率：

```bash
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

## 环境要求

- Go 1.21 或更高版本

## 许可证

本项目采用 Apache License 2.0 许可证 - 详见 [LICENSE](LICENSE) 文件。

## 贡献

欢迎贡献！请随时提交 Pull Request。
