# Security

## Reporting a vulnerability

Please report security problems privately through GitHub: go to the
**Security** tab of this repository and choose **Report a vulnerability**.
Do not open a public issue.

Include what you found, how to reproduce it, and what an attacker could do
with it. You should get a reply within a week.

## Scope

This project starts VMs on your host and registers them as GitHub Actions
runners. Problems worth reporting include:

- a job escaping its VM or reaching the host or other jobs
- runner credentials (JIT configs, the GitHub App key or tokens) leaking
- one job's VM being reused for another job

## Using self-hosted runners with public repositories

GitHub advises against self-hosted runners for public repositories, because
a pull request from a fork can run code on them. If you point this service
at a public repository, require approval for workflow runs from outside
contributors in the repository's Actions settings.
