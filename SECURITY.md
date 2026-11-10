# Security policy

## Supported versions

Only the latest release gets security fixes.

## Reporting a vulnerability

Report privately through GitHub security advisories: open the repository's
Security tab and choose "Report a vulnerability". Please do not put details in a public
issue.

If private reporting isn't available, open an issue asking for a contact,
without details.

Include:

- the version (`uptime-monitor -version`) and how you run it
- the relevant parts of your config, with secrets removed
- steps to reproduce, and what you expected to happen
- the impact as you see it

Expect an acknowledgement within 7 days.

## Scope

In scope: the server and its documented production setup.

Out of scope: a self-hoster's own misconfiguration, for example
`allow_private_targets: all` on a public instance, or not following the
production setup in the README.
