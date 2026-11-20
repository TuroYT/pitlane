# Getting started

## Requirements

- A Linux host with Docker (installed automatically by `install.sh`) and systemd,
  or any Docker host for the Compose setup.
- Privileged containers must be allowed (Pitlane's slots run Docker-in-Docker).
  On Proxmox, an **unprivileged** LXC with `nesting=1,keyctl=1` works.
- Sizing: plan ~2 vCPU and ~4 GB RAM per parallel job, plus disk for caches
  (100–200 GB is comfortable). Fast disks (SSD/NVMe) matter more than anything else.
- Outbound HTTPS to GitHub. **No inbound access from the internet is needed.**

## 1. Install

Pick one:

| Method | Command |
| --- | --- |
| systemd | `curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/install.sh \| sudo sh` |
| Docker Compose | download [`deploy/compose.yaml`](../deploy/compose.yaml), set `PITLANE_BASE_URL`, `docker compose up -d` |
| Proxmox VE | on the Proxmox host: `bash -c "$(curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/pitlane-lxc.sh)"` |

On Proxmox the script is the [community-scripts](https://community-scripts.org/)
installer (sources in [`deploy/proxmox`](../deploy/proxmox)): you pick default or
advanced settings in its menu, or set `var_cpu`, `var_ram`, `var_disk`,
`var_hostname`... before the command (8 cores, 16 GB of RAM and 150 GB of disk by
default). It always installs the latest release.

`PITLANE_BASE_URL` is the address **you** type in your browser, e.g.
`http://pitlane.lan:8080`. GitHub only redirects your browser there, so it can be
a private LAN address.

## 2. Create the GitHub App

Open Pitlane in your browser. It asks for the **setup token**. Pitlane saves it
in the `setup-token` file of its data directory (it is not printed in the logs,
unless that file cannot be written) and removes the file once the first admin
has signed in. Read it with:

```sh
sudo pitlane setup-token                           # systemd install
pct exec <CTID> -- pitlane setup-token             # Proxmox, on the Proxmox host
cat ~/pitlane.creds                                # Proxmox, inside the container
docker compose exec pitlane /pitlane setup-token   # Compose
```

The data directory is only readable by root (inside the Proxmox container you
are root: leave out `sudo`). If you changed `PITLANE_DATA` in
`/etc/pitlane/pitlane.env`, your shell does not have it: pass the directory,
`sudo pitlane setup-token /path/to/data`. If Pitlane could not write the file,
it logs the token instead, as a warning (so at any log level but `error`): look
in the server logs (`sudo journalctl -u pitlane`, `docker compose logs pitlane`).

Click **Create GitHub App on github.com**. GitHub shows the App that will be
created (private to you or your organization). Confirm, and you are sent back to Pitlane.

The App only gets the permissions needed to manage runners:

| Permission | Why |
| --- | --- |
| Repository → Administration: read & write | register runners on repositories |
| Organization → Self-hosted runners: read & write | register runners on organizations |
| Repository → Actions: read, Metadata: read | read job information |

## 3. Install the App and sign in

Click **Install the App on GitHub**, choose the account(s) and repositories, then
**Sign in with GitHub**. The first person to sign in becomes the administrator.
Add more people in **Users**, as admins, operators or viewers, by GitHub login or
by organization or team (see [Users and roles](configuration.md#users-and-roles)).

## 4. Enable repositories

In **Repositories**, click **Enable** next to each repository, or **Enable for the
whole organization**. Pitlane registers a runner scale set named `pitlane` there.

## 5. Use it in workflows

```yaml
jobs:
  test:
    runs-on: pitlane
```

Try [`examples/smoke.yml`](../examples/smoke.yml): it uses a Postgres service,
`npm install` and a `docker build`. Run it twice to see the caches at work.

## Updating

```sh
curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/install.sh | sudo sh   # systemd
docker compose pull && docker compose up -d                                                    # Compose
update                                                                                         # Proxmox, inside the container
```

With Compose, first set the new version in the `image:` tag of `compose.yaml`
(it names an exact release, e.g. `ghcr.io/turoyt/pitlane:v0.2.0`).

`install.sh` takes everything from a single release (set `PITLANE_VERSION=0.2.0`
to choose it) and refuses a binary that does not match that release's
`checksums.txt`. Release binaries and images also carry build provenance, which
you can check with the GitHub CLI:

```sh
gh attestation verify pitlane_linux_amd64 --repo TuroYT/pitlane
gh attestation verify oci://ghcr.io/turoyt/pitlane:v0.2.0 --repo TuroYT/pitlane
```

Restarts are safe: running jobs keep running and are picked up again.
