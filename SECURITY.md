# Security policy

## Supported versions

Security fixes go into the latest minor release only (1.0.x today). Older
lines do not get backports: upgrade with `install.sh`, a new image tag, or the
in-app updater.

| Version | Supported |
| --- | --- |
| Latest minor (1.0.x) | Yes |
| Anything older, release candidates | No |

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private vulnerability reporting:
**[Security → Report a vulnerability](https://github.com/TuroYT/pitlane/security/advisories/new)**.

Include the Pitlane version, how it is installed (systemd, Compose, Proxmox
LXC), the steps or a proof of concept, and what an attacker gains. Redact
tokens, private keys and hostnames you do not want in the report.

Pitlane has a single maintainer, so set your expectations accordingly:

- the maintainer aims to acknowledge a report within a week;
- a confirmed issue is fixed in a patch release as soon as a fix is ready, with
  a GitHub security advisory and credit if you want it;
- please give the fix a reasonable time to ship before you publish details. We
  can agree on a date together.

## Scope

Read [docs/security.md](docs/security.md) first: it describes what Pitlane
protects and what it does not.

**In scope**, for example:

- A job reaching the host's Docker socket, another slot's Docker daemon or
  network, or another GitHub owner's caches, **without** using the privileged
  Docker daemon it is given to break out of its slot.
- A strict job starting with another job's caches, images or slot state, or
  leaving them behind, when no job broke out of its slot.
- Access to the actions cache with a missing, expired or foreign token.
- Authentication or role bypass in the web UI and API: signing in without
  being allowed, a `viewer` or `operator` doing admin actions, any role getting
  a secret back from the API, cross-site request forgery.
- Secrets (GitHub App key, client and webhook secrets, `master.key`) leaking
  through the API, the UI, logs or the audit log.
- The self-update or `install.sh` accepting a binary that does not match the
  release's provenance or checksums.

**Out of scope**, because the docs already say Pitlane does not prevent it:

- A job taking over the host through its privileged Docker daemon, and what
  follows from that (reading the data directory, tampering with other slots,
  removing firewall rules). Privileged Docker-in-Docker is not a security
  boundary, and strict mode is not a sandbox.
- A job reading or poisoning cache entries of another repository or branch of
  the **same** GitHub owner.
- Running untrusted pull requests from forks on Pitlane.
- Anything that needs `--insecure-no-auth` exposed to a network, or an
  attacker who already has admin access.
- Vulnerabilities in GitHub, Docker or the official runner image itself.
  Report those upstream; if Pitlane's pinned version is affected, an issue
  asking for a bump is welcome.

Not sure whether something is in scope? Report it privately anyway.
