# Security Policy

## Supported Versions

Security fixes are issued against the latest stable release. Older patch versions on the same minor track also receive fixes; pre-release tags (`-alpha.N`, `-beta.N`, `-rc.N`) do not.

## Supported Scope

- `server/**`
- `apps/web/svelte/**`
- `apps/desktop/**`
- `apps/ios/**`, `apps/tvos/**`, `apps/apple-core/**`
- `apps/android-tv/**`
- `packaging/windows/**`
- `Dockerfile`, `docker-compose.yml`, `docker/**`
- `.github/workflows/**`

## Reporting a Vulnerability

Please use GitHub Security Advisories for private disclosure:

- [Xuva Security Advisories](https://github.com/EJM-Digital/Xuva/security/advisories)

Do not open public issues for active vulnerabilities.

Include:

- affected route/feature
- reproduction steps
- expected vs actual behavior
- impact assessment

## Repository Hardening Baseline

Current baseline:

- main branch protection enabled
- stale review dismissal enabled
- one approval required for PR merge
- conversation resolution required before merge
- linear history required
- force-push blocked on main
- branch deletion blocked on main
- admins/owner can still push directly when needed
- secret scanning enabled
- push protection enabled
- Dependabot security updates enabled

Detailed policy:

- [Repository Hardening](docs/repository-hardening.md)

