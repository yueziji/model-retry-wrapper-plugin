# Model Retry Wrapper Plugin

[English](README.md)

这是一个基于 Go/cgo 的 CPA 原生动态库插件，演示了一个轻量级的 ModelRouter + executor 包装器。它会在错误返回给下游客户端之前，在 CPA 内部对选定的模型别名进行重试。

## 功能

- 只把显式配置的客户端请求模型名或别名路由到插件执行器。
- 通过 `host.model.execute` 或 `host.model.execute_stream` 调用常规的宿主模型执行路径。
- 在错误返回下游之前，由插件重试已配置的上游 HTTP 状态码。
- 当宿主回调没有暴露数字 HTTP 状态码时，可以按配置的错误关键词做兜底重试。
- 在嵌套的宿主模型回调中跳过自己的路由器，避免包装器递归调用自身。

该插件适合用于显式的重试别名，例如 `retry-codex-gpt-5.5` 或 `retry-claude-sonnet`。除非普通模型名被列入 `models`，否则针对普通模型名的请求不会被插件处理。

## 配置

从 `v0.0.6` 或更新版本的 GitHub Release 下载对应平台的资产，解压后把动态库放到 CPA 的插件目录。动态库文件名主体必须是 `model-retry-wrapper`，这样 CPA 才会把它映射到 `plugins.configs.model-retry-wrapper`。

Release 归档内包含对应平台的文件名：

- Windows 使用 `model-retry-wrapper.dll`
- Linux 使用 `model-retry-wrapper.so`
- macOS 使用 `model-retry-wrapper.dylib`

启用动态插件，并把 `plugins.dir` 指向动态库所在目录：

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
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
      retry_keywords:
        - "rate_limited"
      max_attempts: 0
      initial_delay_ms: 500
      max_delay_ms: 10000
```

`max_attempts` 包含第一次上游请求。`0` 表示不限制尝试次数；下游取消仍会通过 CPA 的宿主回调停止请求，但如果取消发生在 backoff sleep 期间，插件会在下一次宿主回调时才观察到取消，因此停止最多会延迟当前重试等待时间。

`status_codes` 是主要重试规则。`retry_keywords` 只作为兜底，用于宿主回调没有提供数字 HTTP 状态码的错误，例如 `rate_limited`；如果省略该字段，或被 WebUI 保存为空列表，默认兜底关键词仍为 `rate_limited`。

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

构建需要 Go 1.26 或更新版本、启用 CGO，并安装目标平台可用的 C 编译工具链。

在仓库根目录执行：

```bash
go test .
go build -buildmode=c-shared -ldflags "-X main.pluginVersion=0.0.6" -o model-retry-wrapper.dll .
```

请根据目标系统使用对应的平台扩展名：

- Windows 使用 `.dll`
- Linux 使用 `.so`
- macOS 使用 `.dylib`

## 发布

本仓库包含 GitHub Actions 发布工作流。可以手动运行 `Release` 并填写下一个 semver 标签；也可以推送一个标签，例如：

```bash
git tag v0.0.6
git push origin v0.0.6
```

该工作流会运行测试，为 Windows、Linux 和 macOS 构建原生动态库，把标签版本写入插件 metadata，并将 zip 归档发布为 GitHub Release 资产。

## 许可证

MIT
