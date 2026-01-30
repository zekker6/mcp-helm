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
- **get_latest_version_of_chart** - Retrieves the latest version of a specific chart
- **get_chart_values** - Retrieves the values file for a chart (latest version or specific version)
- **get_chart_contents** - Retrieves the contents of a chart (including templates, values, and metadata)
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
docker run -d --name mcp-helm -p 8012:8012 ghcr.io/zekker6/mcp-helm:v1.0.6 -mode=sse
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

> Note: Go 1.24.3 is required.

```bash
go install github.com/zekker6/mcp-helm/cmd/mcp-helm@latest
```

### Build from Source

> Note: Go 1.24.3 is required.

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

### Authentication

The server supports authentication for both OCI registries and HTTP Helm repositories.

#### Command-Line Flags

| Flag                        | Description                                                           |
|-----------------------------|-----------------------------------------------------------------------|
| `-username`                 | Username for authentication (works for both OCI and HTTP repos)       |
| `-password-file`            | Path to file containing password)                                     |
| `-registry-credentials`     | Path to Docker-style credentials file (e.g., `~/.docker/config.json`) |
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
  ghcr.io/zekker6/mcp-helm:latest \
  -mode=sse -username myuser -password-file /secrets/password.txt

# With Docker credentials
docker run -d --name mcp-helm -p 8012:8012 \
  -v ~/.docker/config.json:/root/.docker/config.json:ro \
  ghcr.io/zekker6/mcp-helm:latest \
  -mode=sse
```

## Roadmap

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
