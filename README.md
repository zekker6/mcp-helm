# MCP Helm Server

An MCP (Model Context Protocol) server that provides tools for interacting with Helm repositories and charts. This
server enables AI assistants to query Helm repositories, retrieve chart information, and access chart values without
requiring local Helm installation.

The purpose of using MCP for Helm is to avoid making up format of `values.yaml` and contents of the charts when working
with LLMs.
Instead, the server provides a standardized way to access this information, making it easier for AI assistants to
interact with Helm charts and repositories.

This MCP server is and will be providing tools for working with Helm repositories only. If you need to work with other
Kubernetes resources, consider using a separate MCP server that provides tools for Kubernetes resources.

## Features

The MCP Helm server provides the following tools:

- **list_repository_charts** - Lists all charts available in a Helm repository (or chart name for OCI registries)
- **list_chart_versions** - Lists all available versions/tags for a chart
- **get_latest_version_of_chart** - Retrieves the latest stable (non-prerelease) version of a specific chart
- **get_chart_values** - Retrieves the values file for a chart (latest version or specific version)
- **get_chart_contents** - Retrieves the contents of a chart (including templates, values, and metadata), optionally
  filtered by file path glob patterns (for example, `templates/**`)
- **get_chart_dependencies** - Retrieves the dependencies of a chart as defined in its `Chart.yaml` file
- **get_chart_images** - Extracts container images used in a Helm chart by rendering templates and parsing Kubernetes
  manifests

### Repository Types

All tools support both traditional HTTP Helm repositories and OCI registries:

| Repository Type  | Example URL                        |
|------------------|------------------------------------|
| HTTP Repository  | `https://charts.example.com`       |
| OCI Registry     | `oci://ghcr.io/org/charts/mychart` |
| OCI (Docker Hub) | `oci://docker.io/library/mysql`    |

### OCI Registry Support

OCI (Open Container Initiative) registries store Helm charts as OCI artifacts. Unlike HTTP repositories where multiple
charts share an index, OCI registries typically contain one chart per repository with multiple version tags.

**Example usage with OCI:**

```
repository_url: oci://ghcr.io/nginxinc/charts/nginx-ingress
chart_name: (empty - chart name is in the URL)
```

## Try without installation

There is a publicly available instance of the MCP Helm server that you can use to test the features without installing
it: https://mcp-helm.zekker.dev/mcp

## Installation

### Run with docker

You can run the MCP Helm server using Docker. This is the easiest way to get started without needing to install Go or
build from source.

```bash
docker run -d --name mcp-helm -p 8012:8012 ghcr.io/zekker6/mcp-helm:v1.3.0 -mode=sse
```

Note that the `--mode=sse` flag is used to enable Server-Sent Events mode, which used by MCP clients to connect.
Alternatively, you can use `-mode=http` to enable Streamable HTTP mode.

### Via pre-build binary

Download binary from the [releases page](https://github.com/zekker6/mcp-helm/releases).

Example for Linux x86_64 (note that other architectures and platforms are also available):

```bash
latest=$(curl -s https://api.github.com/repos/zekker6/mcp-helm/releases/latest | grep 'tag_name' | cut -d\" -f4)
wget https://github.com/zekker6/mcp-helm/releases/download/$latest/mcp-helm_Linux_x86_64.tar.gz
tar axvf mcp-helm_Linux_x86_64.tar.gz
```

### Via Mise

Mise ([mise-en-place](https://mise.jdx.dev/)) is a development environment setup tool.

```bash
mise i ubi:zekker6/mcp-helm@latest
```

### Install with Go

> Note: Go 1.26.0 is required.

```bash
go install github.com/zekker6/mcp-helm/cmd/mcp-helm@latest
```

### Build from Source

> Note: Go 1.26.0 is required.

1. Clone the repository:
   ```bash
   git clone https://github.com/zekker6/mcp-helm.git
   cd mcp-helm
   ```

2. Build the binary:
   ```bash
   go build -o mcp-helm ./cmd/mcp-helm
   ```

3. Run the server:
   ```bash
   ./mcp-helm
   ```

## Configuration

Configure your MCP client to connect to this server. The server implements the standard MCP protocol for tool discovery
and execution.

### Repository index caching

The index of an HTTP Helm repository is downloaded on first use and reused for `-repo-index-max-age` (default `5m`).
Once that age is exceeded, the next request downloads the index again, so newly published chart versions show up
without a restart. Set `-repo-index-max-age=0` to download the index on every request. OCI registries are always
queried live.

### Authentication

The server supports authentication for both OCI registries and HTTP Helm repositories.

#### Command-Line Flags

| Flag                        | Description                                                           |
|-----------------------------|-----------------------------------------------------------------------|
| `-username`                 | Username for basic authentication (HTTP repos, and OCI registries not covered by `-registry-credentials`) |
| `-password-file`            | Path to file containing password                                      |
| `-registry-credentials`     | Path to Docker-style credentials file (e.g., `~/.docker/config.json`); authoritative for the OCI registries it lists |
| `-registry-plain-http`      | Use plain HTTP for OCI registries (insecure, for development only)    |
| `-tls-cert`                 | Path to TLS client certificate file for HTTP repositories             |
| `-tls-key`                  | Path to TLS client key file for HTTP repositories                     |
| `-tls-ca`                   | Path to CA certificate file for verifying server certificates         |
| `-tls-insecure-skip-verify` | Skip TLS certificate verification (insecure)                          |
| `-pass-credentials-all`     | Pass credentials to all domains when following redirects              |

#### Basic Authentication

For repositories requiring username/password authentication:

```bash
# Create a password file (recommended for security)
echo "your-password" > /path/to/password.txt
chmod 600 /path/to/password.txt

# Run with basic auth
./mcp-helm -username myuser -password-file /path/to/password.txt
```

#### OCI Registry Authentication

For private OCI registries, authentication can be configured via:

1. **Docker credentials** - The server automatically uses credentials from `~/.docker/config.json`
2. **Explicit credentials file** - Use `-registry-credentials` flag

```bash
# Using Docker login (credentials stored in ~/.docker/config.json)
docker login ghcr.io
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin

# Using explicit credentials file
./mcp-helm -registry-credentials /path/to/docker/config.json

# Using basic auth for OCI registry
./mcp-helm -username myuser -password-file /path/to/password.txt
```

##### Combining basic auth with a registry credentials file

A single instance can serve private HTTP repositories and private OCI registries
at the same time. When both `-username/-password-file` and `-registry-credentials`
are set, OCI requests are routed per registry host:

- If the credentials file resolves a credential for the chart's registry host,
  that per-host credential is used (`auths`, `credHelpers`, and `credsStore` are
  all consulted, using the same Docker credential resolution as the Helm CLI, so
  Docker Hub's canonical `https://index.docker.io/v1/` key is matched correctly).
- Otherwise, the static `-username/-password-file` basic auth is used.

This lets `-registry-credentials` stay authoritative for the OCI registries it
covers while basic auth still applies to HTTP repositories (and any OCI registry
the credentials file does not resolve).

```bash
# HTTP repos use basic auth; OCI hosts in config.json use their per-host creds
./mcp-helm \
  -username myuser -password-file /path/to/password.txt \
  -registry-credentials /path/to/docker/config.json
```

Credentials behind an external credential store (`credsStore`) or per-registry
helper (`credHelpers`) are resolved by invoking that helper binary at runtime.
If the helper is not available in the runtime environment, the affected
registries fall back to basic auth; a warning is logged at startup so this is
visible. Routing considers only the file passed to `-registry-credentials` (no
implicit `~/.docker/config.json` fallback), so list every private OCI registry
you need in that file.

#### TLS/mTLS Configuration

For repositories with custom TLS requirements:

```bash
# Custom CA certificate (for self-signed or internal CAs)
./mcp-helm -tls-ca /path/to/ca.crt

# Client certificate authentication (mTLS)
./mcp-helm -tls-cert /path/to/client.crt -tls-key /path/to/client.key

# Combined: mTLS with custom CA
./mcp-helm -tls-cert client.crt -tls-key client.key -tls-ca ca.crt

# Skip TLS verification (development only, not recommended for production)
./mcp-helm -tls-insecure-skip-verify
```

#### Docker Configuration

Example with Docker, passing authentication:

```bash
# With basic auth
docker run -d --name mcp-helm -p 8012:8012 \
  -v /path/to/password.txt:/secrets/password.txt:ro \
  ghcr.io/zekker6/mcp-helm:v1.3.0 \
  -mode=sse -username myuser -password-file /secrets/password.txt

# With Docker credentials
docker run -d --name mcp-helm -p 8012:8012 \
  -v ~/.docker/config.json:/root/.docker/config.json:ro \
  ghcr.io/zekker6/mcp-helm:v1.3.0 \
  -mode=sse
```

## Observability

The server can export OpenTelemetry traces, metrics and logs over OTLP. It is **disabled by default**: with
`OTEL_ENABLED` unset nothing is exported, no exporter is constructed, no provider is registered globally and no
instrumentation is installed, so the binary behaves exactly as it does today.

`OTEL_ENABLED` is the only switch. `OTEL_SDK_DISABLED` is **not** consulted, and neither is
`OTEL_EXPORTER_OTLP_PROTOCOL`: the wire protocol comes from the endpoint's scheme, described below.

### Environment Variables

| Variable                             | Default        | Description                                                                                          |
|--------------------------------------|----------------|------------------------------------------------------------------------------------------------------|
| `OTEL_ENABLED`                       | `false`        | Master switch. Telemetry is set up only when this holds a true-ish value (`true`, `1`, `t`)           |
| `OTEL_EXPORTER_OTLP_ENDPOINT`        | -              | Base endpoint for all three signals. Required when enabled, unless every per-signal endpoint is set   |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | base endpoint  | Traces-only override, used verbatim (include the full path for HTTP)                                 |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`| base endpoint  | Metrics-only override, used verbatim                                                                 |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`   | base endpoint  | Logs-only override, used verbatim                                                                    |
| `OTEL_SERVICE_NAME`                  | `mcp-helm`     | Service name reported to the collector                                                               |
| `OTEL_RESOURCE_ATTRIBUTES`           | -              | Extra resource attributes, e.g. `deployment.environment.name=production`                                  |
| `OTEL_EXPORTER_OTLP_HEADERS`         | -              | Headers sent with every export, e.g. for authentication. Read by the SDK exporters                   |
| `OTEL_EXPORTER_OTLP_TIMEOUT`         | `10000`        | Export timeout in milliseconds. Read by the SDK exporters                                            |
| `OTEL_EXPORTER_OTLP_COMPRESSION`     | -              | Set to `gzip` to compress exports. Read by the SDK exporters                                         |
| `OTEL_METRIC_EXPORT_INTERVAL`        | `60000`        | Metric export interval in milliseconds                                                               |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`     | system roots   | PEM CA bundle used to verify a `https://` or `grpcs://` collector. Read by the SDK exporters         |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` | -           | PEM client certificate for mTLS to the collector. Read by the SDK exporters                          |
| `OTEL_EXPORTER_OTLP_CLIENT_KEY`      | -              | PEM client key for mTLS to the collector. Read by the SDK exporters                                  |

When telemetry is enabled and an endpoint is missing or malformed, the server exits non-zero with an error naming the
offending variable; a typo never degrades silently into "no telemetry". With `OTEL_ENABLED` off the endpoint variables
are not parsed at all, so a stale value in the environment cannot break startup.

### Resource Attributes

The server sets `service.name` (from `OTEL_SERVICE_NAME`, else `mcp-helm`) and `service.version` itself, and reads
anything else from `OTEL_RESOURCE_ATTRIBUTES`. The semantic conventions call for more than that, and the rest has to
come from the deployment:

| Attribute                    | Where it should come from                                                             |
|------------------------------|---------------------------------------------------------------------------------------|
| `deployment.environment.name`| `OTEL_RESOURCE_ATTRIBUTES`. Note the `.name` suffix - bare `deployment.environment` is deprecated |
| `service.instance.id`        | `OTEL_RESOURCE_ATTRIBUTES` from the downward API. Required once more than one replica runs, or every instance's series collide |
| `service.namespace`          | `OTEL_RESOURCE_ATTRIBUTES`, when other services share the backend                     |
| `k8s.pod.uid` and other `k8s.*` | The Collector's `k8sattributes` processor, which is the supported way to add them   |

On Kubernetes, the first two come from the downward API:

```yaml
env:
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
  - name: OTEL_RESOURCE_ATTRIBUTES
    value: deployment.environment.name=production,service.instance.id=$(POD_NAME)
```

### Endpoint Schemes

The wire protocol and TLS both come from the endpoint's URL scheme:

| Endpoint                        | Protocol  | TLS | Exports go to                                                       |
|---------------------------------|-----------|-----|---------------------------------------------------------------------|
| `grpc://collector:4317`         | OTLP/gRPC | no  | `collector:4317`                                                    |
| `grpcs://otel.example.com:4317` | OTLP/gRPC | yes | `otel.example.com:4317`                                             |
| `http://collector:4318`         | OTLP/HTTP | no  | `http://collector:4318/v1/traces`, `/v1/metrics`, `/v1/logs`        |
| `https://otel.example.com`      | OTLP/HTTP | yes | `https://otel.example.com/v1/traces`, `/v1/metrics`, `/v1/logs`     |

Any other scheme is a startup error naming the four accepted schemes.

- The **base** endpoint is a base URL: for HTTP the signal path (`/v1/traces`, `/v1/metrics`, `/v1/logs`) is appended,
  after trimming a trailing slash. A collector endpoint injected with a trailing slash is handled.
- A **per-signal** endpoint is used verbatim, as the OTLP specification requires, so for HTTP it has to carry the full
  path including `/v1/traces` and friends.
- gRPC endpoints keep only `host:port`; gRPC has no signal path, so any path is dropped.
- The protocol is resolved per signal, so mixed configurations work: traces over gRPC while metrics and logs go over
  HTTP is a supported setup.

### Configuration Examples

One endpoint for all three signals:

```bash
OTEL_ENABLED=true \
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
./mcp-helm -mode=http
```

Traces, metrics and logs are sent to `http://otel-collector:4318/v1/traces`, `/v1/metrics` and `/v1/logs`. Switching
that endpoint to `grpc://otel-collector:4317` moves all three signals to OTLP/gRPC without any other change.

Per-signal endpoints, mixing protocols and TLS:

```bash
OTEL_ENABLED=true \
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=grpc://otel-collector:4317 \
OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=https://metrics.example.com/v1/metrics \
OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://logs.example.com/v1/logs \
OTEL_SERVICE_NAME=mcp-helm \
OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=production \
./mcp-helm -mode=http
```

With Docker:

```bash
docker run -d --name mcp-helm -p 8012:8012 \
  -e OTEL_ENABLED=true \
  -e OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318 \
  ghcr.io/zekker6/mcp-helm:latest -mode=http
```

The `OTEL_*` variables are ignored by images built before OpenTelemetry support landed, so pin a tag that has it or
use `latest`.

### What Is Emitted

#### Traces

A tool call over HTTP produces one trace from the inbound request down to the Helm work it triggers:

```
POST /mcp                       (HTTP server span)
  mcp.tools/call                (MCP protocol span)
    tool.get_chart_images       (MCP tool span)
      helm.get_chart_images     (Helm operation span)
        helm.load_chart
          helm.oci.pull
        helm.parse.images
```

The Helm-specific attributes below are written without their `cloud.zekker.helm.` prefix for readability - the emitted
names carry it. That is this project's namespace: the OpenTelemetry naming rules reserve unprefixed names for the
specification, and no registry attribute covers a Helm repository or chart. Attributes that do come from the registry
(`url.full`, `server.address`, `server.port`, `error.type`, `gen_ai.*`, `mcp.*`) are shown in full.

| Span                          | Kind     | Attributes                                                                                   |
|-------------------------------|----------|----------------------------------------------------------------------------------------------|
| `<METHOD> <route>`            | server   | `otelhttp` HTTP server semantic conventions. Only in `sse` and `http` modes                  |
| `mcp.<method>`                | server   | `mcp.method`, `mcp.session.id`, `mcp.protocol.version` (set by mcp-go)                       |
| `tool.<name>`                 | internal | `gen_ai.tool.name`, `gen_ai.operation.name`, `mcp.method.name`                                |
| `helm.list_charts`            | internal | `repository.url`, `repository.type`, `chart.count`                                            |
| `helm.list_chart_versions`    | internal | + `chart.name`, `version.count`                                                               |
| `helm.get_latest_version`     | internal | + `chart.name`, `chart.version` (resolved)                                                    |
| `helm.get_latest_values`      | internal | + `chart.name`, `chart.version` (resolved). Library-only; no MCP tool reaches it              |
| `helm.get_chart_values`       | internal | + `chart.name`, `chart.version`                                                               |
| `helm.get_chart_contents`     | internal | + `chart.name`, `chart.version`, `recursive`                                                  |
| `helm.get_chart_dependencies` | internal | + `chart.name`, `chart.version`, `dependency.count`                                           |
| `helm.get_chart_images`       | internal | + `chart.name`, `chart.version`, `recursive`, `image.count`                                   |
| `helm.load_chart`             | internal | `repository.type`, `chart.name`, `chart.version`                                              |
| `helm.oci.pull`               | client   | `oci.ref`, `server.address`, `server.port`                                                    |
| `helm.oci.tags`               | client   | `oci.ref`, `server.address`, `server.port`                                                    |
| `helm.repo.index`             | client   | `repository.url`, `server.address`, `server.port`                                             |
| `helm.chart.download`         | client   | `url.full`, `server.address`, `server.port`                                                   |
| `helm.parse.images`           | internal | `recursive`                                                                                   |
| `helm.parse.contents`         | internal | `recursive`                                                                                   |

A failing span is marked with an `ERROR` status, records the exception and carries `error.type`. URL attributes are
sanitized: `user:password@` userinfo is stripped before a repository, OCI or chart URL becomes a span attribute.

The `helm.chart.download` span reports `url.full` because the chart URL is the exact resource fetched.
`helm.repo.index` does not: the Helm getter resolves the index path itself, so the URL this server holds is not the one
requested.

mcp-go labels its own spans with `mcp.method` and `mcp.tool.name`, which the registry has since deprecated in favour of
`mcp.method.name` and `gen_ai.tool.name`. The current names are added to the `tool.<name>` span by this server, so both
are present there; `mcp.<method>` spans carry only what mcp-go sets.

Incoming W3C `traceparent` / `tracestate` headers are honoured in `sse` and `http` modes, so a tool call joins the
caller's trace instead of starting a new one. In `stdio` mode each MCP message is a root span.

#### Metrics

| Metric                                  | Kind      | Unit | Attributes                                                                   |
|-----------------------------------------|-----------|------|------------------------------------------------------------------------------|
| `mcp.server.operation.duration`         | histogram | `s`  | `mcp.method.name`, `gen_ai.tool.name`, `gen_ai.operation.name`, `error.type` |
| `cloud.zekker.helm.operation.duration`  | histogram | `s`  | `…helm.operation`, `…helm.repository.type`, `error.type`                     |
| `http.server.*`                | from `otelhttp` | - | `http.server.request.duration`, `http.server.request.body.size`, `http.server.response.body.size`. `sse`/`http` modes only |
| `go.*`                         | from the OTel runtime instrumentation | - | Go runtime memory, GC and goroutine metrics |

`mcp.server.operation.duration` is the histogram the MCP semantic conventions define for the receiving side.

`error.type` is **absent on success**, which is what the conventions specify - do not filter on an `ok` value, filter on
the attribute being unset. On a failure it holds the Go error type, except for a tool handler that reported the failure
to its caller rather than returning an error (mcp-go delivers that as a successful response carrying an error result),
where it is `tool_error`. A tool handler that panicked reports `_OTHER`. The histograms' `_count` series double as call
rates, so there is no separate counter.

Helm operations nest, and each level records its own point: `get_latest_values` wraps `get_latest_version` and
`get_chart_values`, and any tool call that omits `chart_version` records `get_latest_version` before the operation it
was asked for. Filter by `cloud.zekker.helm.operation` rather than summing `_count` across operations, or one logical
call is counted more than once.

Metric attributes are deliberately low cardinality: repository URLs, chart names and chart versions appear on spans
only, never on a metric. On the `http.server.*` metrics, `server.address` and `server.port` report `-httpListenAddr`
rather than the request's `Host` header, so an unauthenticated probe varying that header cannot open a series per
value.

#### Logs

With telemetry enabled, log records are teed to the OTLP log exporter in addition to stderr. Both sinks honour
`-logLevel`, so raising it keeps the filtered records off the wire as well as out of stderr. Each tool call also logs
one INFO record with the tool name, duration and, on failure, `error.type`.

Every record emitted from a traced call site is correlated with its span, so logs, traces and metrics line up in the
backend. The two sinks carry that correlation differently: the stderr line gets `trace_id` and `span_id` fields, while
the exported record gets the log data model's own trace id fields, which the SDK fills in from the emitting context.
The ids are stripped from the exported record's attributes so they are not on the wire twice.

### Limitations

Two things are deliberately not instrumented:

- **No outbound HTTP client spans.** Helm's SDK builds its own request contexts internally and its transport option
  takes a concrete `*http.Transport`, so an instrumented HTTP client would emit orphaned root spans disconnected from
  the tool's trace. Outbound work is covered by the manual `helm.repo.index`, `helm.chart.download`, `helm.oci.pull`
  and `helm.oci.tags` client spans instead.
- **Long-lived stream requests are excluded from HTTP traces and metrics.** The `GET /sse` request in `sse` mode and the
  `GET /mcp` request in `http` mode stay open for the life of the client session. Tracing them would produce hours-long
  spans and put session durations into the request-latency histogram, so both are filtered out. The JSON-RPC requests
  carried over those sessions are traced normally.

## Shutdown

On `SIGTERM` or `SIGINT` the server stops accepting new work, drains in-flight requests within 10 seconds and then
flushes buffered telemetry within 5, so a client that never disconnects or a collector that never answers cannot keep
the process alive. The budgets are separate on purpose: a drain that runs long cannot spend the time the flush needs.
A second signal terminates immediately.

Give the process room to finish: set `terminationGracePeriodSeconds` (or `docker stop -t`) to at least 20 seconds, or
the spans and log records buffered at the moment of the signal are lost.

A transport that fails to start - a port already in use, an unusable listen address - is logged as `fatal` and exits
with status 1.

## Roadmap

- [x] OpenTelemetry instrumentation (traces, metrics and logs over OTLP)
- [x] Add more tools
    - [x] List all charts in a repository
    - [x] List all versions of a chart
    - [x] Get latest version of the chart
    - [x] Get values for chart
    - [x] Get values for the latest version of the chart
    - [x] Extract full chart content
    - [x] Extract dependant charts from Charts.yaml
    - [x] Extract images used in chart
- [x] Support OCI registries
    - [x] Pull charts from OCI registries
    - [x] List tags/versions from OCI registries
    - [x] Support authentication via Docker credentials
- [x] Support using private HTTP repositories
    - [x] Add a way to provide credentials for HTTP basic auth
