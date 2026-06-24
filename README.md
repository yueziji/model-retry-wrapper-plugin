# Model Retry Wrapper Plugin

[中文](README.zh-CN.md)

This Go/cgo native plugin demonstrates a small ModelRouter + executor wrapper for retrying selected model aliases inside CLIProxyAPI (CPA) before an error reaches the downstream client.

## What It Does

- Routes only explicitly configured client-requested model names or aliases to the plugin executor.
- Calls the normal host model execution path through `host.model.execute` or `host.model.execute_stream`.
- Retries configured upstream HTTP status codes in the plugin before returning an error downstream.
- Can optionally retry configured error-message keywords when the host callback does not include a numeric HTTP status.
- Skips its own router on nested host model callbacks, so the wrapper does not recurse into itself.

The plugin is intended for explicit retry aliases such as `retry-codex-gpt-5.5` or `retry-claude-sonnet`. Requests for normal model names are left untouched unless those names are listed in `models`.

## Configuration

Download a `v0.0.7` or newer release asset for your platform, extract the dynamic library, and place it under CPA's plugin directory. The library basename must be `model-retry-wrapper` so CPA maps it to `plugins.configs.model-retry-wrapper`.

Release archives contain the expected platform filename:

- `model-retry-wrapper.dll` on Windows
- `model-retry-wrapper.so` on Linux
- `model-retry-wrapper.dylib` on macOS

Enable dynamic plugins and point `plugins.dir` at the directory containing the library:

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

`max_attempts` includes the first upstream attempt. `0` means no attempt cap. Downstream cancellation stops requests through CPA host callbacks; streaming startup retries also probe the downstream plugin stream during backoff so canceled streams stop instead of continuing the retry loop.

`status_codes` is the primary retry rule. `retry_keywords` is only a fallback for host callback errors that do not expose a numeric HTTP status, such as `rate_limited`; if omitted or saved as an empty list, the default fallback is `rate_limited`.

## Alias Pattern

Configure the retry alias only on the credential group you want to target:

```yaml
codex-api-key:
  - api-key: "sk-..."
    models:
      - name: "gpt-5.5"
        alias: "retry-codex-gpt-5.5"
```

Then request the alias:

```json
{
  "model": "retry-codex-gpt-5.5",
  "messages": [
    {"role": "user", "content": "Hello"}
  ]
}
```

The plugin matches the alias, wraps the host execution path, and lets the existing CPA auth selection and translators do the actual provider call.

## Streaming Behavior

Streaming retries are safe only before the first payload is emitted downstream. This plugin starts the host stream, reads until it sees the first payload, and retries configured startup errors. Once the first payload is emitted to the downstream client, later stream errors are forwarded as stream errors instead of being retried.

## Build

Builds require Go 1.26 or newer, CGO enabled, and a C toolchain for the target platform.

From this repository root:

```bash
go test .
go build -buildmode=c-shared -ldflags "-X main.pluginVersion=0.0.7" -o model-retry-wrapper.dll .
```

Use the platform extension expected by your target system:

- `.dll` on Windows
- `.so` on Linux
- `.dylib` on macOS

## Release

This repository includes a GitHub Actions release workflow. Run `Release` manually with the next semver tag, or push a tag such as:

```bash
git tag v0.0.7
git push origin v0.0.7
```

The workflow runs tests, builds Windows/Linux/macOS dynamic libraries, injects the tag version into plugin metadata, and publishes zip archives as GitHub Release assets.

## License

MIT
