# Getting help

## Where to ask

- **Questions, setup help, ideas**: [GitHub Discussions](https://github.com/TuroYT/pitlane/discussions).
  Search first: someone may have hit the same thing.
- **A bug** (Pitlane does something wrong, or a job fails only on Pitlane): open a
  [bug report](https://github.com/TuroYT/pitlane/issues/new?template=bug_report.yml).
- **A feature request**: open a
  [feature request](https://github.com/TuroYT/pitlane/issues/new?template=feature_request.yml),
  or start a discussion if the idea is still rough.
- **A security problem**: never in public. Use
  [private vulnerability reporting](https://github.com/TuroYT/pitlane/security/advisories/new),
  as described in the [security policy](https://github.com/TuroYT/pitlane/blob/main/SECURITY.md).

Pitlane has one maintainer. Answers can take a few days, and a clear first
message saves a round trip.

Before asking, [Troubleshooting](troubleshooting.md) covers the common
problems: jobs stuck in the queue, slots in `error`, a full disk, sign-in
redirect errors and multi-arch builds.

## What to include

- The Pitlane version (bottom of the UI, or `pitlane version`).
- How you installed it: `install.sh` (systemd), Docker Compose, the Proxmox LXC
  script, or built from source.
- Where it runs: LXC container, VM or bare metal, the host OS and kernel
  (`uname -r`), and the Docker version (`docker version`).
- The cgroup version: `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs` for v2,
  `tmpfs` for v1.
- What you did, what you expected and what happened. A link to the workflow run
  helps, and so does the job's YAML.
- Logs from around the time of the problem:

  ```sh
  journalctl -u pitlane --since "1 hour ago"    # systemd
  docker compose logs --since 1h pitlane        # Compose
  docker logs pitlane-slot-0                    # a slot in error
  ```

  Set `PITLANE_LOG_LEVEL=debug` for more detail.

**Redact logs before you paste them.** Remove tokens (the setup token, runner
and cache tokens), the GitHub App's client and webhook secrets, private keys,
and hostnames or repository names you want to keep private.
