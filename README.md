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

### Install from a custom CPA plugin source

CPA versions that support `plugins.store-sources` can discover this plugin, check its latest GitHub release, and install or update it from the Management Center. Add this repository's registry URL to CPA:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/yueziji/model-retry-wrapper-plugin/master/registry.json"
```

Open the CPA plugin store, select **Model Retry Wrapper**, and install or update it. Releases `v0.0.13` and newer publish the CPA-standard platform archives and `checksums.txt` required for verified installation. CPA reports when a newer release is available; applying the update remains an explicit Management Center action rather than an unattended background replacement.

After installation, configure at least one model under `plugins.configs.model-retry-wrapper.models` as shown below. Without a configured model, the plugin intentionally routes no requests.

### Manual installation

Download a `v0.0.11` or newer release asset for your platform, extract the dynamic library, and place it under CPA's plugin directory. The library basename must be `model-retry-wrapper` so CPA maps it to `plugins.configs.model-retry-wrapper`.

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
      max_elapsed_time_ms: 0
```

`max_attempts` includes the first upstream attempt; `0` removes the attempt-count cap. `max_elapsed_time_ms` limits the wall-clock duration of each complete retry sequence; `0` (the default) removes that limit. Setting both fields to `0` retries until success or external cancellation. A positive elapsed-time limit stops backoff or prevents the next attempt, but it cannot preempt a host callback that is already running. `initial_delay_ms` and `max_delay_ms` must be greater than `0`, `max_elapsed_time_ms` must be `0` or greater, and the initial delay must not exceed the maximum delay. In particular, `max_delay_ms: 0` does not mean unlimited.

Downstream cancellation stops requests through CPA host callbacks. On hosts that support plugin schema version 2, the plugin registers both a request interceptor and a request lifecycle listener. The interceptor carries CPA's host-generated `RequestID` to this plugin's executor through an internal header, which is removed before the nested host model call is sent upstream. Each executor registers its own cancelable context under that ID, so a terminal `canceled`/`failed`/`rejected` event cancels only the matching retry sequence; a short-lived pending cancellation also covers an event that races ahead of executor registration. The retry wait is context-aware and does not write probe chunks into the downstream stream. Older schema-1 hosts keep working unchanged but cannot provide this exact asynchronous correlation. If a downstream client only stops consuming while keeping the HTTP connection open, CPA has no cancellation signal and an unbounded retry sequence will continue; configure `max_attempts` or `max_elapsed_time_ms` for a hard upper bound. Plugin shutdown cancels its background streams and waits for them to stop calling the host. A host callback that is already running cannot be preempted by the plugin, but cancellation prevents another retry after that callback returns. Go also does not guarantee that a `c-shared` library can be safely hot-unloaded in every process.

`status_codes` is the primary retry rule. `retry_keywords` is only a fallback for host callback errors that do not expose an explicit HTTP status, such as `rate_limited`. Omit `retry_keywords` to use the default `rate_limited` fallback; set `retry_keywords: []` to disable keyword retries and use status codes only.

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

The startup debug log and subsequent stream error logs include diagnostics in the message text so that CPA's fixed-field log formatter displays them:

- `attempt`, `status`, `entry_protocol`, and `exit_protocol` identify the attempt, error or startup status, and protocols.
- `received_chunks` / `received_bytes` and `emitted_chunks` / `emitted_bytes` count observed nonempty payloads and payloads whose host emit calls succeeded. `emit_started=true` means a downstream write was attempted, not that the client received it.
- `payloads` summarizes sizes and known event types for the first 8 nonempty payloads. Inspection is capped at 16 KiB and 8 distinct event labels per payload. Content, tool arguments, response IDs, and arbitrary upstream event names are never included. `samples_truncated=true` means later payloads were not sampled; a payload's `truncated=true` marks the byte or label cap. Events split across payloads are not reassembled and may appear as `sse.partial_frame`, `incomplete_or_unclassified_json`, or `unclassified`, so a summary cannot establish that the entire stream contained no actual content.
- `retry_skipped=downstream_started` explains why a forwarding error is not retried. Emit failures use `downstream_emit_failed`; forwarding errors before any write attempt use `stream_forwarding`. `keyword_match` only reports a configured substring match. `startup_retry_eligible` reports whether the status, keyword, and attempt-count rules would allow retrying a startup error; it does not indicate an actual retry or account for cancellation and elapsed-time limits.

Diagnostics do not buffer or rewrite forwarded content or extend the retry window.

## Build

Builds require Go 1.26 or newer, CGO enabled, and a C toolchain for the target platform.

From this repository root:

```bash
go test .
go build -buildmode=c-shared -o model-retry-wrapper.dll .
```

Use the platform extension expected by your target system:

- `.dll` on Windows
- `.so` on Linux
- `.dylib` on macOS

## Release

This repository includes a GitHub Actions release workflow. Run `Release` manually with the next semver tag, or push a tag such as:

```bash
git tag v0.0.11
git push origin v0.0.11
```

The workflow runs tests, builds Windows/Linux/macOS dynamic libraries, injects the tag version into plugin metadata, and publishes zip archives as GitHub Release assets.

## License

MIT
