# Model Retry Wrapper Plugin

[中文](README.zh-CN.md)

This Go/cgo native plugin demonstrates a small ModelRouter + executor wrapper for retrying selected model aliases inside CLIProxyAPI (CPA) before an error reaches the downstream client.

## What It Does

- Routes only explicitly configured client-requested model names or aliases to the plugin executor.
- Calls the normal host model execution path through `host.model.execute` or `host.model.execute_stream`.
- Retries configured upstream HTTP status codes in the plugin before returning an error downstream.
- Can optionally retry configured error-message keywords even when the HTTP status does not match the configured status codes.
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

`status_codes` and `retry_keywords` are alternative retry rules: a matching HTTP status or error-message keyword allows a retry, subject to the attempt limit, elapsed-time limit, cancellation, and streaming boundary. Keywords use case-insensitive substring matching and apply even when an explicit HTTP status is outside `status_codes`; for example, HTTP 400 with `rate_limited` in the error can be retried without adding 400 to `status_codes`. Omit `retry_keywords` to use the default `rate_limited` keyword; set `retry_keywords: []` to disable keyword retries and use status codes only. Set `status_codes: []` to use keywords only, or set both lists to `[]` to disable both rules.

Retry, stop, and canceled-wait log messages include `attempt`, `status`, `status_match`, `keyword_match`, `retry_allowed`, `stop_reason`, `max_attempts`, `max_elapsed_time_ms`, and `retry_keywords_count`. These values use the effective per-model policy and appear in the message text so CPA's text formatter displays them. `stop_reason` is `no_matching_rule`, `max_attempts`, `canceled`, or `deadline_exceeded`; `none` means this decision allows retrying. A deadline can come from the retry time limit or a parent context. The diagnostic summary excludes upstream error bodies and keyword values. A new host may supply a structured HTTP status even when the error message contains no status number.

### Per-model retry settings

Add `model_overrides` under `plugins.configs.model-retry-wrapper` to change individual retry settings for selected models:

```yaml
models: [retry-codex-gpt-5.5, retry-claude-sonnet]
max_attempts: 5
initial_delay_ms: 500
max_delay_ms: 10000
max_elapsed_time_ms: 60000
model_overrides:
  retry-codex-gpt-5.5:
    max_attempts: 3
    initial_delay_ms: 1000
    retry_keywords: []
```

Here the Codex alias gets up to three attempts, a one-second initial delay, and no keyword retries. Its other fields inherit the global values; the Claude alias uses all global values. The six retry fields (`status_codes`, `retry_keywords`, `max_attempts`, `initial_delay_ms`, `max_delay_ms`, and `max_elapsed_time_ms`) can be overridden independently. Omitted or `null` fields inherit; a supplied list replaces the global list, and `[]` disables that rule. Zero still means unlimited for attempts and elapsed time; `max_attempts: 1` disables additional retries.

Keys use the same client-requested model name or alias as `models`, ignoring case and surrounding whitespace. Blank keys and duplicate normalized keys are rejected. `models` still determines which requests the plugin handles: adding only an override does not enable routing. Removing a model's override, using `{}` for its policy, or clearing `model_overrides` restores inheritance. The complete effective policy is validated at configuration time, including the initial/maximum delay relationship. Each request takes one policy snapshot, used by both normal execution and stream startup retries. The streaming retry boundary is unchanged.

The matching management frontend update offers a per-model form with global inheritance, custom values, unlimited limits, disabled list rules, and a restore-global action. Advanced JSON remains available and other management frontends can use the object editor. Enter the following **inside the `model_overrides` JSON field**:

```json
{
  "retry-codex-gpt-5.5": {
    "max_attempts": 3,
    "initial_delay_ms": 1000,
    "retry_keywords": []
  }
}
```

The JSON field is saved as one complete object, so retain entries for other models when editing it. This feature requires updating the plugin; older plugin versions do not implement `model_overrides`.

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

## Using with CodexComp

The wrapper declares native `codex` input/output support. Set **this wrapper's priority higher than CodexComp's** (for example, `30` if CodexComp uses `1`), and include the requested model in both plugins' model lists. The intended order is client → retry wrapper → CodexComp → native model executor. The wrapper adds a per-request `X-Model-Retry-Wrapper-Applied: 1` header to its host callbacks and declines requests carrying that marker, including CodexComp continuation callbacks. This prevents the two routers from repeatedly wrapping each other. A lower wrapper priority can still wrap CodexComp twice; use the documented order to avoid that extra fold.

The request body, `encrypted_content`, `prompt_cache_key`, and entry/exit protocols are preserved. In Responses/Codex streams, a first `response.failed` or `error` event can trigger the existing status/keyword retry rules before anything is emitted. Inspection holds only an incomplete first event, up to 16 KiB; a normal first event or keepalive ends inspection immediately. Non-retryable errors and exhausted attempts are forwarded unchanged. Error details are used for keyword matching in memory and are omitted from retry diagnostics.

CPA's `codex.stream-bootstrap-buffering` may be enabled independently. CPA can retry startup failures inside each continuation round; the outer wrapper never restarts a response after its first payload is emitted. Initial SSE events can arrive later with CPA buffering enabled.

## Build

Builds require Go 1.26 or newer, CGO enabled, and a C toolchain for the target platform.

The plugin uses CPA SDK `v7.3.20`, native ABI 1, and RPC schema up to 6. Registration negotiates the lower supported schema; hosts omitting a schema retain schema-1 behavior, and request lifecycle capabilities require schema 2 or newer. Schema 6 changes management JSON response escaping and does not alter this wrapper's execution contract. RPC errors use the SDK envelope and preserve structured HTTP status through wrapped errors. Legacy errors without a status still support the existing explicit-status-text and keyword matching rules. The wrapper keeps its own status wrapper where an original Go error must remain available through `errors.Is` / `errors.As`.

From this repository root:

```bash
go test -mod=readonly ./...
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
