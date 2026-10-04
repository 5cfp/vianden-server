# Hosting a Vianden server

This guide sets up a server that your friends can reach over the internet, with real HTTPS, automatic database backups, and automatic restarts. It covers:

- **[A home server](#home-server)**: Ubuntu, your own domain, Docker. This is the main path.
- **[A VPS](#vps)**: the same steps, minus the router and the dynamic IP.
- **[Running without Docker](#without-docker)**: systemd.
- **[What is encrypted (and what is not)](#encryption-model)**: please read this before inviting people.

Time needed: about 30-60 minutes the first time.

---

## What you get
```
          internet
              │  ports 80 + 443 (TCP)
     ┌────────┴────────┐
     │  your router    │  port forwarding
     └────────┬────────┘
              │
   ┌──────────┴─────────── Ubuntu server ───────────────┐
   │  docker compose:                                   │
   │    server   HTTPS (Let's Encrypt), API, WebSocket  │
   │    db       PostgreSQL (not reachable from outside)│
   │    backup   daily database backups → ./backups     │
   └────────────────────────────────────────────────────┘
```

## Requirements
- A machine running **Ubuntu 22.04 or 24.04** (other Linux distributions work similarly), always on, ideally wired to the router.
- A **domain** you control, for example `example.com`. The server will use a name like `chat.example.com`.
- Access to your **router's settings** (for port forwarding).
- A **public IP address** at home. See step 1.

---

## Home server

### 1. Check that your home has a public IP (no CGNAT)
Some internet providers share one public IP between many customers ("CGNAT"). Then nobody can reach your home from outside, whatever you configure.

1. Open your router's status page and find its **WAN / internet IP**.
2. On the server, run: `curl -4 ifconfig.me`
3. **Same address**: good, continue. **Different**, or the router shows an address starting with `100.64.` to `100.127.`: you are behind CGNAT. Ask your provider for a public IP, or use a [VPS](#vps) instead.

### 2. Give the server a fixed address on your home network
In the router's DHCP settings, add a **reservation** for the server (often called "static lease" or "address reservation"), for example `192.168.1.20`. Port forwarding (step 4) needs an address that does not change.

### 3. Point your domain at your home (DNS)
At your DNS provider, create an **A record**:

| Type | Name | Value | TTL |
|---|---|---|---|
| A | `chat` | your public IP from step 1 | 300 (5 minutes) |

If your DNS provider offers a "proxy" option (for example Cloudflare's orange cloud), turn it **off** ("DNS only"): the server must receive connections directly to get its certificate and to handle WebSockets.

Check it from any computer: `nslookup chat.example.com` should show your public IP. DNS changes can take a few minutes.

#### Your home IP changes (dynamic IP)
Most home connections get a new public IP from time to time. When that happens, the A record must be updated, or friends can no longer connect. Let a small program do it automatically:

- **Easiest, if your router supports it:** many routers have a "Dynamic DNS" (DDNS) page with built-in support for common providers. Use it if your DNS provider is listed.
- **Otherwise, `ddclient` on the server** (supports Cloudflare, Namecheap, GoDaddy, Porkbun, deSEC, and many more):
  ```bash
  sudo apt install ddclient
  sudo nano /etc/ddclient.conf
  ```
  Example for a domain whose DNS is at Cloudflare (create an API token with "Zone: DNS: Edit" permission for that zone only):
  ```
  daemon=300
  ssl=yes
  use=web, web=ipify-ipv4
  protocol=cloudflare
  zone=example.com
  login=token
  password=YOUR_CLOUDFLARE_API_TOKEN
  chat.example.com
  ```
  Then `sudo chmod 600 /etc/ddclient.conf` (it contains a secret) and `sudo systemctl restart ddclient`. Check with `sudo ddclient -query` and `journalctl -u ddclient`.
  For other providers, see the examples in `/usr/share/doc/ddclient/`.

### 4. Forward ports on your router
In the router's "Port forwarding" (or "Virtual server" / "NAT") page, forward to the server's fixed address from step 2:

| External port | Protocol | Internal IP | Internal port | Why |
|---|---|---|---|---|
| 443 | TCP | 192.168.1.20 | 443 | HTTPS: the app and its live connection |
| 80 | TCP | 192.168.1.20 | 80 | Let's Encrypt's domain check, and redirects to HTTPS |

Do **not** forward any other ports (especially not 5432, the database, or 22, SSH).

> Voice channels (a later version) will need one extra UDP port. This guide will say so when it is time.

### 5. Prepare Ubuntu
```bash
sudo apt update && sudo apt upgrade -y
# Security updates installed automatically:
sudo apt install -y unattended-upgrades git
sudo dpkg-reconfigure -plow unattended-upgrades
```

Install Docker (official instructions: https://docs.docker.com/engine/install/ubuntu/), short version:
```bash
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER   # use docker without sudo; log out and in again afterwards
docker compose version          # should print a version
```

**Firewall note.** If you use `ufw`: Docker's published ports (here 80 and 443) bypass `ufw` rules. That is fine for this setup, because only 80 and 443 are published on purpose and the database is not published at all. Keep SSH restricted, for example `sudo ufw allow from 192.168.1.0/24 to any port 22`.

### 6. Download and configure the server
```bash
git clone https://github.com/5cfp/vianden-server.git
cd vianden-server/deploy
cp .env.example .env
openssl rand -hex 32        # copy the output: this is your database password
nano .env
```
In `.env`, set:
- `VIANDEN_SERVER_NAME`: the name your friends see.
- `VIANDEN_DOMAIN`: `chat.example.com`.
- `VIANDEN_ACME_EMAIL`: optional; Let's Encrypt writes to it if something is wrong with your certificate.
- `DB_PASSWORD`: the random value from `openssl rand -hex 32`.

Protect it: `chmod 600 .env`. It contains the database password; never share it or commit it.

### 7. Start
```bash
docker compose up -d --build
docker compose ps            # after ~30 s: server and db should be "healthy"
docker compose logs server   # the server's messages
```

The first HTTPS request makes the server get its certificate from Let's Encrypt (a few seconds). Open `https://chat.example.com/api/v1/info` in a browser: you should see JSON with your server name and **no** certificate warning.

### 8. Become the owner
The server prints a one-time **setup token** while it has no owner:
```bash
docker compose logs server | grep vo_
```
In the app: enter `chat.example.com`, choose **Register**, and paste the token as the invite code. You are now the owner. The token stops working immediately.

### 9. Invite your friends
1. Send them the Windows app (the `vianden-client_..._windows_x64.zip` from a release, with its SHA-256 checksum so they can check the download: `Get-FileHash file.zip` in PowerShell).
   The app is not code-signed yet, so Windows SmartScreen may warn on first start: **More info → Run anyway**.
2. In the app: **⋯** (bottom left) → **Invite a friend** creates a code for one person, valid 7 days.
3. They enter `chat.example.com`, choose **Register**, and use the code.

Test from **outside** your network (for example a phone on mobile data) at least once. Connecting to your own domain from inside your home network can fail on some routers (no "NAT loopback"). If that happens only at home, add `192.168.1.20 chat.example.com` to the hosts file of your home PCs (`C:\Windows\System32\drivers\etc\hosts`).

---

## Everyday operation

### Updating
```bash
cd vianden-server
git pull
cd deploy
docker compose pull            # newer PostgreSQL images (security fixes)
docker compose up -d --build    # rebuilds the server, restarts what changed
docker image prune -f           # removes old, unused images
```
Database changes ("migrations") are applied automatically when the new version starts. Make sure a recent backup exists first (see below).

Do this regularly (for example monthly), even without a new Vianden version: `docker compose pull` is how the database gets its security updates.

### Logs and status
```bash
docker compose ps                     # health of each part
docker compose logs -f server         # follow the server log (Ctrl+C to stop)
docker compose logs server | grep "failed login"   # password guessing attempts
```

### Backups
The `backup` container saves the whole database every 24 hours into `deploy/backups/` and keeps 14 days (both configurable in `.env`).

**A backup on the same disk is not enough.** If the disk dies, the backups die with it. Copy them regularly to another place: another disk, another machine, or cloud storage. Backups contain **all messages and the password hashes**, so treat them like the database itself. Encrypt copies that leave your home, for example with [age](https://github.com/FiloSottile/age):
```bash
age -p -o vianden-backup.dump.age deploy/backups/vianden-2026-10-04_120000.dump
```

**Restoring** a backup (replaces ALL current data):
```bash
cd vianden-server/deploy
docker compose stop server
docker compose exec db sh -c 'dropdb -U "$POSTGRES_USER" "$POSTGRES_DB" && createdb -U "$POSTGRES_USER" "$POSTGRES_DB"'
docker compose exec backup sh -c 'pg_restore --dbname="$PGDATABASE" --no-owner /backups/vianden-2026-10-04_120000.dump'
docker compose start server
```
Try a restore once while nothing important is at stake, so you know it works.

### Third-party licenses
The server image contains the license texts of all included open-source code in `/licenses`. Release archives include them in `third_party_licenses/`. The app shows them under **⋯ → About & licenses**.

---

## Encryption model

Please read this, and tell your users the short version.

**In transit (always):**
- Everything between the app and the server (login, messages, the live connection) goes over **HTTPS/TLS** (TLS 1.2 or newer). People on the same Wi-Fi, the internet provider, or anyone on the way **cannot read or change it**.
- Voice (a later version) will be encrypted in transit too (DTLS-SRTP, part of WebRTC).

**On the server (not end-to-end):**
- The encryption is **between each app and the server**, not from one user to another. The server decrypts everything it receives. Messages are stored **unencrypted** in the database, and so are the backups.
- So **whoever controls the server (you, the host) can technically read all messages.** So can anyone who steals the server's disk or a backup. This is normal for self-hosted chat servers (each community trusts its own server), but users deserve to know it.
- **Passwords** are never stored; only Argon2id hashes are. **Login tokens** are stored only as SHA-256 hashes.

**What you should do:**
- **Use full-disk encryption** on the server. On Ubuntu, choose "Encrypt the new Ubuntu installation" (LUKS) during installation. This protects the data if the machine or disk is stolen (not while it is running).
- Keep backups safe and encrypted when they leave the machine (see [Backups](#backups)).
- Keep Ubuntu updated (`unattended-upgrades`, step 5), and use SSH keys, not passwords.

Encrypting stored files and end-to-end encrypted voice are planned for a later version.

---

## VPS
A rented virtual server works the same way, with fewer steps:

1. Rent a small VPS with Ubuntu 24.04 (1 CPU and 1-2 GB RAM is plenty for a group of friends). It has a **fixed public IP**, so skip steps 1, 2, and the dynamic DNS part of step 3.
2. Step 3: point the A record to the VPS's IP.
3. Step 4: instead of the router, open **TCP 80 and 443** in the provider's firewall (if it has one). Do not open 5432.
4. Steps 5-9 are identical.

The server program and the `docker-compose.yml` are exactly the same on a VPS and at home; only `.env` differs.

---

## No domain? (self-signed mode)
Without a domain, Let's Encrypt cannot issue a certificate. Set in `.env`:
```
VIANDEN_TLS_MODE=self-signed
VIANDEN_DOMAIN=203.0.113.7     # your public IP (or leave empty)
```
The server creates its own certificate and prints its **fingerprint** at every start:
```bash
docker compose logs server | grep -A1 "fingerprint is"
```
Send the fingerprint to your friends through a different channel than the server address (for example a text message). The app shows it the first time they connect and asks them to compare. It remembers the certificate and warns loudly if it ever changes. With a dynamic home IP, friends must type a new address whenever your IP changes, so a domain is much more convenient.

Keep the `serverdata` Docker volume: it holds the certificate. A new certificate means every user sees a "certificate changed" warning.

---

## Without Docker
For a Linux machine where you prefer a plain service:

1. Install PostgreSQL 16 or newer (`sudo apt install postgresql`), then create a user and database:
   ```bash
   sudo -u postgres createuser --pwprompt vianden
   sudo -u postgres createdb --owner=vianden vianden
   ```
2. Download a release (`vianden-server_<version>_linux_amd64.tar.gz`), or build one with `./scripts/build-release.sh`, and install the program:
   ```bash
   sudo install -m 755 vianden-server /usr/local/bin/
   sudo useradd --system --no-create-home --shell /usr/sbin/nologin vianden
   ```
3. Configuration: `sudo mkdir /etc/vianden`, then create `/etc/vianden/server.env` from `.env.example` with `VIANDEN_TLS_MODE=autocert`, `VIANDEN_DOMAIN`, and `VIANDEN_DATABASE_URL=postgres://vianden:PASSWORD@localhost:5432/vianden?sslmode=disable`. Then `sudo chmod 600 /etc/vianden/server.env`.
4. Service: `sudo cp deploy/vianden-server.service /etc/systemd/system/`, then `sudo systemctl enable --now vianden-server`. Logs: `journalctl -u vianden-server -f`.
5. Backups: schedule `pg_dump --format=custom` with cron or a systemd timer (see `deploy/backup.sh` for the idea).

---

## Troubleshooting

| Problem | What to check |
|---|---|
| `https://chat.example.com` shows a certificate error, or nothing | `docker compose logs server`. Usual causes: the A record does not point to your current public IP (`nslookup`), port **80** is not forwarded (Let's Encrypt checks over port 80), or you are behind CGNAT (step 1). |
| Logs mention "too many failed authorizations" or a rate limit | Let's Encrypt allows only a few failed attempts per hour. Fix DNS or port 80 first, wait an hour, then restart: `docker compose restart server`. |
| `bind: address already in use` | Something else uses port 80 or 443 on the server (for example another web server). Stop it, or move it. |
| Friends cannot connect, but it works for you at home | Test from mobile data. Check port forwarding and the router's firewall, and that the A record shows your current IP. |
| Works from outside, but not from inside your home | Your router has no NAT loopback: see the hosts-file tip in step 9. |
| `server` is "unhealthy" | `docker compose logs server`. Often the database password in `.env` was changed after the database was first created; it only takes effect on a fresh database volume. |
