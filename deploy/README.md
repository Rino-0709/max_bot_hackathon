# Server Deployment

Target: Ubuntu 22.04/24.04 VPS with Docker Compose.

Recommended server:

- 2 vCPU
- 2 GB RAM
- 30 GB SSD
- Ubuntu 24.04 LTS

## 1. Prepare Server

Run as a sudo-capable user:

```bash
bash deploy/server-setup.sh
```

The script installs Docker, enables the Docker service, creates `/opt/spring-code-1`, and opens only SSH via UFW if UFW is available.

## 2. Upload Project

From local Windows PowerShell:

```powershell
.\deploy\upload.ps1 -ServerHost "SERVER_IP" -User "root"
```

For a non-standard SSH port:

```powershell
.\deploy\upload.ps1 -ServerHost "SERVER_IP" -User "root" -Port 2222
```

## 3. Configure Environment

On server:

```bash
cd /opt/spring-code-1
cp .env.production.example .env
nano .env
```

Set at least:

- `BOT_TOKEN`
- `TECH_ADMIN_USER_IDS`
- `POSTGRES_PASSWORD`
- `GRAFANA_ADMIN_PASSWORD`

Leave monitoring binds as `127.0.0.1` unless Grafana is protected by VPN or nginx auth.

## 4. Start

```bash
cd /opt/spring-code-1
docker compose up -d --build
docker compose ps
```

## 5. Check

```bash
docker compose logs -f bot
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/metrics | head
```

Prometheus:

```bash
curl -fsS http://127.0.0.1:9090/-/healthy
```

Grafana is available on the server itself at:

```text
http://127.0.0.1:3000
```

For local access without opening Grafana to the internet:

```powershell
ssh -L 3000:127.0.0.1:3000 root@SERVER_IP
```

Then open:

```text
http://localhost:3000
```

## 6. Backup

On server:

```bash
cd /opt/spring-code-1
bash deploy/backup-postgres.sh
```

Backups are written to:

```text
/opt/spring-code-1/backups
```

## 7. Restore

Copy backup file to the server and run:

```bash
cd /opt/spring-code-1
bash deploy/restore-postgres.sh backups/backup-file.sql.gz
```

Restore drops and recreates the application database. Use only during maintenance.
