# Contributing to llmproxy

Thank you for your interest in contributing to llmproxy! This document covers the essentials of building, testing, and contributing to the project.

## Project Layout

llmproxy consists of two main parts:

1. **`llmproxyd` daemon** (`main.go`) — A Go HTTP/HTTPS/WebSocket reverse proxy that intercepts and logs LLM API traffic
2. **`llmproxy` wrapper** (`llmproxy`) — A bash script that launches the daemon, configures environment variables, and runs your coding agent

Additional components:

- **`lib/`** — Optional C helper libraries (`intercept.c`, `redirect.c`) for MITM/TLS interception of stubborn static binaries
- **`examples/`** — Example scripts and usage patterns
- **`web/inspect.html`** — Dashboard for viewing captured logs
- **`logs/`** — Runtime directory where captured prompts and traffic are stored

## Building

Requirements:

- Go 1.26 or later
- gcc (only needed for the optional C libs in `lib/`)

To build the project:

```bash
make build
```

This compiles the Go daemon to `llmproxyd` and builds the C library to `lib/intercept`.

## Running Tests

The project uses Go's standard testing framework. To run all tests:

```bash
go test ./...
```

Tests should pass before submitting a pull request.

## Coding Style

We follow idiomatic Go conventions. Before submitting code:

```bash
go vet ./...
```

Please ensure:
- Code is properly formatted (`go fmt ./...`)
- Public functions have godoc comments
- Error handling is explicit and clear
- Variable naming follows Go conventions

## Adding Support for a New Coding Agent

To add support for a new coding agent:

1. **Launcher detection** — In `llmproxy` (the bash script), add a detection case for the agent command and set the appropriate environment variables (e.g., `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, `GEMINI_API_BASE`)

2. **Daemon handling** — In `main.go`, ensure the proxy correctly forwards the API endpoints used by the agent (Anthropic, OpenAI, or Gemini formats)

3. **Testing** — Run the agent through llmproxy and verify that:
   - Traffic is intercepted and logged
   - System prompts are extracted correctly
   - The agent functions normally without errors

4. **Documentation** — Update the README's "Supported Agents" table with the new entry

## Pull Request Workflow

1. Fork the repository on GitHub
2. Create a branch for your work (`git checkout -b my-feature`)
3. Commit your changes with clear, focused messages
4. Push to your fork (`git push origin my-feature`)
5. Open a pull request against the `main` branch

Keep PRs focused on a single issue or feature. Write clear commit messages (imperative mood, single-line subject) and include relevant context in your PR description.

## Reporting Bugs / Requesting Features

Please use GitHub Issues to report bugs or request features. When reporting a bug, include:

- Steps to reproduce
- Expected vs actual behavior
- Your environment (OS, Go version, agent being proxied)
- Relevant logs (sanitize any API keys)

For feature requests, describe the use case and proposed behavior clearly.

## Questions?

Feel free to open an issue for any questions about contributing or using llmproxy.
