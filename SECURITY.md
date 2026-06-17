# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in llmproxy, please report it privately rather than opening a public issue.

**Preferred method**: Use GitHub's private vulnerability reporting feature:
1. Navigate to the [Security tab](https://github.com/ViperBlackSkull/llmproxy/security)
2. Click "Report a vulnerability"
3. Submit details about the vulnerability

**Expectations**: The maintainer (ViperBlackSkull) aims to acknowledge reports within approximately 72 hours. You will receive a response indicating whether the vulnerability is accepted and an estimated timeline for a fix.

**Please do not open public issues** for security bugs. Responsible disclosure helps protect users.

**Scope**: This policy covers:
- The `llmproxyd` daemon (Go proxy server)
- The `llmproxy` launcher (wrapper CLI)
- The release pipeline and CI configuration

## Supported Versions

Only the latest release line is actively supported for security updates. Users are encouraged to update to the most recent release.

## Intended Use & Authorization

llmproxy is a **localhost development tool** designed for inspecting your own LLM API traffic.

**Purpose**: This tool helps developers and researchers monitor, debug, and understand the LLM API requests made by coding agents running on their own machines.

**MITM/TLS Interception**: The man-in-the-middle and TLS interception features (`lib/intercept.c`, `lib/redirect.c`, and examples such as `examples/grab.py`) are intended for **authorized testing and research** on systems you own or are explicitly authorized to test.

**Legal and ethical use**:
- Only intercept traffic from applications and systems you own or have permission to test
- Do not use llmproxy to intercept traffic without authorization
- Unauthorized interception of network traffic may be unlawful in your jurisdiction

This tool is **not designed for malicious purposes** such as eavesdropping on third-party communications or bypassing security controls on systems you do not own.

For additional details, see the [Security Notice](README.md#security-notice) in the README.

## Hardening Notes

llmproxy prioritizes simplicity for local development use. Consider the following:

**Inspect Dashboard**: The inspect dashboard (`http://localhost:8777/__inspect__`) binds to localhost and has no authentication. Do not expose it to a network or the internet. Use it only on a trusted local machine.

**API Keys**: API keys are read from environment variables (e.g., `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) and are never hardcoded in the application. Keep your `.env` files out of version control (they are included in `.gitignore` by default).

**Network Exposure**: The proxy daemon binds to `127.0.0.1` by default. Avoid configuring it to listen on external interfaces unless you understand the security implications and have implemented appropriate access controls.
