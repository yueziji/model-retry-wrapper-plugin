# Model Retry Wrapper Plugin

[English](README.md)

这是一个纯 Go 插件，演示了一个轻量级的 ModelRouter + executor 包装器。它会在错误返回给下游客户端之前，在 CPA 内部对选定的模型别名进行重试。

## 功能

- 只把显式配置的客户端请求模型名或别名路由到插件执行器。
- 通过 `host.model.execute` 或 `host.model.execute_stream` 调用常规的宿主模型执行路径。
- 在错误返回下游之前，由插件重试已配置的上游 HTTP 状态码。
- 在嵌套的宿主模型回调中跳过自己的路由器，避免包装器递归调用自身。

该插件适合用于显式的重试别名，例如 `retry-codex-gpt-5.5` 或 `retry-claude-sonnet`。除非普通模型名被列入 `models`，否则针对普通模型名的请求不会被插件处理。

## 配置

```yaml
plugins:
  configs:
    model-retry-wrapper:
      enabled: true
      priority: 30
      models:
        - "retry-codex-gpt-5.5"
        - "retry-claude-sonnet"
      source_formats:
        - "openai"
        - "claude"
      status_codes: [408, 429, 500, 502, 503, 504]
      max_attempts: 0
      initial_delay_ms: 500
      max_delay_ms: 10000
```

`max_attempts` 包含第一次上游请求。`0` 表示一直重试，直到客户端取消请求。

## 别名模式

只在你希望命中的凭据组上配置重试别名：

```yaml
codex-api-key:
  - api-key: "sk-..."
    models:
      - name: "gpt-5.5"
        alias: "retry-codex-gpt-5.5"
```

然后请求该别名：

```json
{
  "model": "retry-codex-gpt-5.5",
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}
```

插件会匹配这个别名，包装宿主执行路径，并让现有的 CPA 认证选择和协议转换器完成实际的提供商调用。

## 流式行为

流式重试只有在第一个 payload 被发送给下游之前才是安全的。该插件会启动宿主流，读取到第一个 payload 为止，并重试配置范围内的启动错误。一旦第一个 payload 已发送给下游客户端，后续流错误会作为流错误继续转发，而不会再次重试。

## 构建

在仓库根目录执行：

```bash
go test .
go build -buildmode=c-shared -o model-retry-wrapper-go.dll .
```

请根据目标系统使用对应的平台扩展名：

- Windows 使用 `.dll`
- Linux 使用 `.so`
- macOS 使用 `.dylib`

## 发布

本仓库包含 GitHub Actions 发布工作流。可以手动运行 `Release`，并在首次发布时保留默认版本 `v0.0.1`；也可以推送一个标签，例如：

```bash
git tag v0.0.1
git push origin v0.0.1
```

该工作流会为 Windows、Linux 和 macOS 构建原生插件归档，并将它们发布为 GitHub Release 资产。
