# Security Policy

## Supported Versions

| Version | Supported |
| ------- | --------- |
| Latest release | ✅ |
| Older releases | ❌ |

We support only the latest released version of GopherClaw. Please upgrade before reporting a vulnerability.

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report vulnerabilities privately via [GitHub's private vulnerability reporting](https://github.com/EMSERO/gopherclaw/security/advisories/new).

Include as much detail as possible:
- Description of the vulnerability
- Steps to reproduce
- Potential impact
- Any suggested mitigations

You can expect an acknowledgment within **48 hours** and a status update within **7 days**.

## Scope

Security-relevant areas of GopherClaw include:

- **Authentication** — API keys, tokens, and session handling
- **Telegram/HTTP channel security** — message routing, access control
- **Skill execution** — subprocess execution, command injection
- **Configuration** — secrets stored in config files or environment variables
- **Self-update mechanism** — binary download and verification

## Out of Scope

- Vulnerabilities in third-party dependencies (please report upstream)
- Bugs without a security impact
- Social engineering

## Disclosure

We follow responsible disclosure. Once a fix is available, we will:
1. Release a patched version
2. Publish a GitHub Security Advisory with full details
3. Credit the reporter (unless they prefer to remain anonymous)
