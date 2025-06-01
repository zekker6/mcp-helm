# MCP Helm Server

An MCP (Model Context Protocol) server that provides tools for interacting with Helm repositories and charts. This
server enables AI assistants to query Helm repositories, retrieve chart information, and access chart values without
requiring local Helm installations.

## Features

The MCP Helm server provides the following tools:

- **list_repository_charts** - Lists all charts available in a Helm repository
- **get_latest_version_of_chart** - Retrieves the latest version of a specific chart
- **get_chart_values** - Retrieves the values file for a chart (latest version or specific version)

## Installation

### Prerequisites

- Go 1.24.3 or later for building from source

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
    - [ ] Extract dependant charts from Charts.yaml
    - [ ] Extract images used in chart
