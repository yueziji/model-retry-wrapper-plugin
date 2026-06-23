# Model Retry Wrapper Plugin

This Go-only plugin demonstrates a small ModelRouter + executor wrapper for retrying selected model aliases inside CPA before an error reaches the downstream client.

## What It Does

- Routes only explicitly configured client-requested model names or aliases to the plugin executor.
- Calls the normal host model execution path through `host.model.execute` or `host.model.execute_stream`.
- Retries configured upstream HTTP status codes in the plugin before returning an error downstream.
- Skips its own router on nested host model callbacks, so the wrapper does not recurse into itself.

The plugin is intended for explicit retry aliases such as `retry-codex-gpt-5.5` or `retry-claude-sonnet`. Requests for normal model names are left untouched unless those names are listed in `models`.

## Configuration

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

`max_attempts` includes the first upstream attempt. `0` means retry until the client cancels the request.

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

From this repository root:

```bash
go test .
go build -buildmode=c-shared -o model-retry-wrapper-go.dll .
```

Use the platform extension expected by your target system:

- `.dll` on Windows
- `.so` on Linux
- `.dylib` on macOS

## Release

This repository includes a GitHub Actions release workflow. Run `Release` manually and keep the default version `v0.0.1` for the first release, or push a tag such as:

```bash
git tag v0.0.1
git push origin v0.0.1
```

The workflow builds native plugin archives for Windows, Linux, and macOS, then publishes them as GitHub Release assets.
