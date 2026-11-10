# Security policy

## Supported versions

Only the latest release gets security fixes.

## Reporting a vulnerability

Report privately through GitHub security advisories: open the repository's
Security tab and choose "Report a vulnerability". Please do not open a public
issue.

Include:

- the version (`uptime-monitor -version`) and how you run it
- the relevant parts of your config, with secrets removed
- steps to reproduce, and what you expected to happen
- the impact as you see it

Expect an acknowledgement within 7 days.

## Scope

In scope: the server and its default configuration.

Out of scope: a self-hoster's own misconfiguration, for example
`allow_private_targets: all` on a public instance, or exposing the server
without a TLS-terminating proxy.
