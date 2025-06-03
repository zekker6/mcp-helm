# MCP Helm Server

An MCP (Model Context Protocol) server that provides tools for interacting with Helm repositories and charts. This
server enables AI assistants to query Helm repositories, retrieve chart information, and access chart values without
requiring local Helm installations.

## Features

The MCP Helm server provides the following tools:

- **list_repository_charts** - Lists all charts available in a Helm repository
- **get_latest_version_of_chart** - Retrieves the latest version of a specific chart
- **get_chart_values** - Retrieves the values file for a chart (latest version or specific version)
- **get_chat_contents** - Retrieves the contents of a chart (including templates, values, and metadata).

## Installation

### Prerequisites

- Go 1.24.3 or later for building from source

### Run with docker

You can run the MCP Helm server using Docker. This is the easiest way to get started without needing to install Go or
build from source.

```bash
docker run -d --name mcp-helm -p 8012:8012 --command ghcr.io/zekker6/mcp-helm:v0.0.2 --mode=sse
```

Note that the `--mode=sse` flag is used to enable Server-Sent Events mode, which used by MCP clients to connect.

### Via pre-build binary

Download binary from the [releases page](https://github.com/zekker6/mcp-helm/releases).

Example for Linux x86_64 (note that other architectures and platforms are also available):

```bash
latest=$(curl -s https://api.github.com/repos/zekker6/mcp-helm/releases/latest | grep 'tag_name' | cut -d\" -f4)
wget https://github.com/zekker6/mcp-helm/releases/download/$latest/mcp-helm_Linux_x86_64.tar.gz
tar axvf mcp-helm_Linux_x86_64.tar.gz
```

### Install with Go

```bash
go install github.com/zekker6/mcp-helm@latest
```

### Build from Source

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

## Roadmap

- [ ] Add more tools
    - [x] List all charts in a repository
    - [x] Get latest version of the chart
    - [x] Get values for chart
    - [x] Get values for the latest version of the chart
    - [x] Extract full chart content
    - [ ] Extract dependant charts from Charts.yaml
    - [ ] Extract images used in chart
- [ ] Support using private registries
    - [ ] Add a way to provide credentials
