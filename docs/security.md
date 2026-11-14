# Security

## Isolation model

- Jobs **never** get the host's Docker socket. Each slot runs its own Docker
  daemon (Docker-in-Docker, privileged container), and a job only controls that
  daemon.
- Because slot daemons are privileged, a malicious job could escape to the Pitlane
  host. **Treat the host as dedicated to CI**, ideally a VM or an LXC container used
  only for Pitlane.
- With the default cache scope (`owner`), caches never cross GitHub owners: a slot
  that switches to another user or organization is wiped first.
- **Do not enable public repositories** unless you understand the risk: anyone can
  open a pull request that runs code on your runners. The UI flags public repositories.

## Secrets

- The GitHub App private key, client secret and webhook secret are encrypted with
  AES-256-GCM using `master.key` from the data directory.
- Just-in-time runner configs are passed to the runner container's environment and
  are single-use.
- UI sessions use random 256-bit tokens in `HttpOnly`, `SameSite=Lax` cookies
  (`Secure` when the base URL is HTTPS). State-changing API calls require a custom
  header, which blocks cross-site requests.

## Access

- The first setup is protected by a one-time token, available only on the host.
- Only GitHub logins listed in **Allowed GitHub users** can sign in.
- Put Pitlane behind HTTPS (a reverse proxy) if it is reachable outside your LAN.

## Reporting a vulnerability

Please open a private security advisory on GitHub
(**Security → Report a vulnerability**) rather than a public issue.
