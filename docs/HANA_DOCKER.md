# SAP HANA express in Docker (test node)

How the HANA target used for xpro-oss testing and benchmarks was set up on the
test node (Debian 12, 16 vCPU / 30 GB), and how to run,
stop and remove it. HANA ran as a container on the **root** Docker daemon.

## Why a container, and why the root daemon

* The binary installer (`hxe.tgz`, `setup_hxe.sh`) installs system-wide (`/hana`,
  `/usr/sap`, `hxeadm`/`sapadm` users), only recognises SLES/RHEL/Ubuntu, and
  needs `/bin/sh` switched from dash to bash. It was downloaded but **not**
  installed; the download was deleted.
* The Docker Hub image `saplabs/hanaexpress` is self-contained and easy to remove.
  Its documented run command is `sudo docker run ...` and sets kernel parameters
  with `--sysctl` / `--ulimit`, which rootless Docker cannot do. The test node's
  existing app stack runs on `admin`'s *rootless* daemon, so HANA was deliberately
  put on the separate root daemon.
* The image doc only lists SLES 12 as tested; Debian 12 worked.

Image tag used: `saplabs/hanaexpress:2.00.088.00.20251110.1` (same as `latest`).
Licence: the run command passes `--agree-to-sap-license`; SAP's developer /
HANA express licences were accepted by the user.

## Host prerequisites

Kernel parameters the image documentation asks for, as a drop-in
(`/etc/sysctl.d/99-hana.conf`, applied with `sysctl -p`):

```
fs.file-max=20000000
fs.aio-max-nr=262144
vm.memory_failure_early_kill=1
vm.max_map_count=135217728
net.ipv4.ip_local_port_range=40000 60999
```

These are host-wide. Delete the file (and reboot or reset the values) to undo.

Data directory and master password (HANA's `hxeadm` is uid 12000, group 79):

```bash
sudo mkdir -p /data/hxe && sudo chown 12000:79 /data/hxe && sudo chmod 700 /data/hxe
# password policy: >= 8 chars, upper + lower + digit, no ` $ \ ' " , no dictionary words
printf '{"master_password" : "%s"}' "$PW" | sudo tee /data/hxe/password.json >/dev/null
sudo chown 12000:79 /data/hxe/password.json && sudo chmod 600 /data/hxe/password.json
```

HANA deletes `password.json` after startup. The generated master password is
kept root-only on the node (`/root/hxe-master-pwd`); it is not in this repo.

## Run

```bash
sudo docker run -d --name hxe \
  -p 39013:39013 -p 39017:39017 -p 39041-39045:39041-39045 \
  -p 1128-1129:1128-1129 -p 59013-59014:59013-59014 \
  -v /data/hxe:/hana/mounts \
  --ulimit nofile=1048576:1048576 \
  --sysctl kernel.shmmax=1073741824 \
  --sysctl net.ipv4.ip_local_port_range='40000 60999' \
  --sysctl kernel.shmmni=32768 \
  --sysctl kernel.shmall=8388608 \
  saplabs/hanaexpress:2.00.088.00.20251110.1 \
  --passwords-url file:///hana/mounts/password.json \
  --agree-to-sap-license
```

**Gotcha:** the image doc says `kernel.shmmni=524288`. On this kernel (6.1) that
fails container creation (`failed to write sysctl kernel.shmmni ... invalid
argument`); the kernel caps it lower, so `32768` was used.

First start takes ~3 minutes; `docker logs hxe` ends with `Startup finished!`
and the container becomes `healthy`.

## Connect

| Item | Value |
|---|---|
| System DB | `jdbc:sap://<host>:39017/?databaseName=SYSTEMDB` |
| Tenant DB `HXE` | `jdbc:sap://<host>:39041/?databaseName=HXE` (SQL port, instance 90) |
| Admin user | `SYSTEM` (password = master password) |
| App user | `XPRO` (own schema `XPRO`), created with `CREATE USER XPRO PASSWORD ... NO FORCE_FIRST_PASSWORD_CHANGE` |

`hdbsql` is inside the container under
`/hana/shared/HXE/exe/linuxx86_64/HDB_*/hdbsql`:

```bash
sudo docker exec -e P="$PW" hxe bash -c \
  '$(ls -d /hana/shared/HXE/exe/linuxx86_64/HDB_*/hdbsql) -i 90 -d HXE -u SYSTEM -p "$P" "select * from M_DATABASE"'
```

xpro-oss target config (`sslmode: disable` because it is localhost):

```yaml
target:
  db: hana
  host: localhost
  port: 39041
  user: XPRO
  password: "${DB_PASSWORD}"
  database: HXE
  schema: XPRO
  sslmode: disable
```

Limits: `maximum_connections` is 65,536 (10 reserved). xpro-oss opens about one
connection per worker.

## Stop, restart, remove

```bash
sudo docker stop -t 300 hxe     # give HANA time to shut down cleanly
sudo docker start hxe
# remove completely:
sudo docker rm -f hxe && sudo docker rmi saplabs/hanaexpress:2.00.088.00.20251110.1
sudo rm -rf /data/hxe /etc/sysctl.d/99-hana.conf
```

**Note:** on 2026-10-14 `hxe` was stopped with the default 10 s timeout and exited
with code 137 (SIGKILL), i.e. HANA did not finish a graceful shutdown. HANA
normally recovers from its log on the next start, but use `-t 300` to avoid it.

## Lessons / gotchas

* `sudo sysctl` and `ufw` are not on the node's PATH/installed; kernel values were
  read from `/proc/sys`. There is no host firewall (`iptables` INPUT is ACCEPT);
  any port restriction would be in the cloud provider's firewall.
* SAP lists 16 GB RAM as the minimum for HANA express (server only). The 23
  application containers on the same node were stopped while HANA ran.
* `M_CONNECTIONS` also lists history rows; filter `CONNECTION_TYPE not like
  'History%'` to count live connections.
