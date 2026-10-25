# Proxmox LXC installer

Scripts for [community-scripts](https://community-scripts.org) (the former tteck scripts), which create a Proxmox VE LXC container and install bitnews in it. User documentation is in [docs/deploy.md](../../docs/deploy.md#proxmox-lxc).

| File | Becomes |
|---|---|
| `ct/bitnews.sh` | `ct/bitnews.sh`: asks for the container settings, creates the LXC, and holds the update function |
| `install/bitnews-install.sh` | `install/bitnews-install.sh`: runs inside the new container and installs bitnews |

Both load the community-scripts helper functions (`build.func`, `fetch_and_deploy_gh_release`, ...), so they only run from that project's repository. They install from the GitHub release of this repository: the asset `bitnews_<version>_linux_amd64.tar.gz` made by `scripts/package-release.sh` and published by `.github/workflows/release.yml` on a version tag. There must be at least one release before the scripts work.

## Install command

Once the scripts are in ProxmoxVE, this is the command to run in the Proxmox VE host shell (the one every community-scripts page shows):

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/community-scripts/ProxmoxVE/main/ct/bitnews.sh)"
```

While a script is still in DevScripts for testing, the maintainers give the exact test command in the issue and Discord thread; use that one.

## Submitting

New scripts go to [community-scripts/DevScripts](https://github.com/community-scripts/DevScripts) first, not to ProxmoxVE (pull requests with new scripts there are closed). Read its contributing guide and the [documentation](https://community-scripts.org/docs/contribution) for the current rules, then:

1. Fork DevScripts and copy `ct/bitnews.sh` and `install/bitnews-install.sh` to the same paths there.
2. Test both on a real Proxmox VE host (the scripts here were only run against a Debian 13 container with stubbed helper functions and no systemd).
3. Open a pull request, then report the result of the test in the project's Discord thread. After that the maintainers promote the script to ProxmoxVE. The name, description, logo and tags are entered on their website, not in the repository.

Everything in the project's repository is MIT licensed, so the two scripts carry that header, while bitnews itself is GPL-3.0.
