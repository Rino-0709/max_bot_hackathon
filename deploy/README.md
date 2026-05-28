# Деплой на сервер

Документ описывает перенос проекта на Ubuntu-сервер с Docker Compose.

Рекомендуемая конфигурация:

- 2 vCPU;
- 2 GB RAM;
- 30 GB SSD или больше;
- Ubuntu 22.04/24.04 LTS.

## 1. Подготовить сервер

Запустите от пользователя с правами `sudo`:

```bash
bash deploy/server-setup.sh
```

Скрипт устанавливает Docker, включает сервис Docker, создаёт каталог
`/opt/spring-code-1` и, если установлен UFW, оставляет открытым только SSH.

## 2. Загрузить проект

С локального компьютера в Windows PowerShell:

```powershell
.\deploy\upload.ps1 -ServerHost "SERVER_IP" -User "root"
```

Если SSH работает на нестандартном порту:

```powershell
.\deploy\upload.ps1 -ServerHost "SERVER_IP" -User "root" -Port 2222
```

## 3. Настроить окружение

На сервере:

```bash
cd /opt/spring-code-1
cp .env.example .env
nano .env
```

Минимально нужно заполнить:

- `BOT_TOKEN`;
- `TECH_ADMIN_USER_IDS`;
- `QR_SECRET`;
- `SCANNER_ACCESS_TOKEN`;
- `SCANNER_TEST_ACCESS_TOKEN`;
- `SCANNER_PUBLIC_URL`;
- `SCANNER_BIND=0.0.0.0`, если публичный HTTPS-сканер должен быть доступен с телефона;
- `SCANNER_FALLBACK_BIND=127.0.0.1`, чтобы запасные порты `8081` и `8443` не открывались наружу;
- `POSTGRES_PASSWORD`;
- `GRAFANA_ADMIN_PASSWORD`.

Мониторинг лучше оставить привязанным к `127.0.0.1`, если Grafana не закрыта VPN,
nginx-авторизацией или другим контролем доступа.

## 4. Запустить сервисы

```bash
cd /opt/spring-code-1
docker compose up -d --build
docker compose ps
```

## 5. Проверить работу

```bash
docker compose logs -f bot
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/metrics | head
```

Prometheus:

```bash
curl -fsS http://127.0.0.1:9090/-/healthy
```

Тестовый QR-сканер:

```text
http://127.0.0.1:8080/scanner
```

Публичный proxy для QR-сканера, если `SCANNER_BIND=0.0.0.0`:

```text
https://scanner.aksenovaks.online/scanner
```

Запасные адреса `http://127.0.0.1:8081/scanner` и `https://127.0.0.1:8443/scanner` оставлены для локальной диагностики на сервере.

Grafana на самом сервере:

```text
http://127.0.0.1:3000
```

Чтобы открыть Grafana с локального компьютера и не публиковать её в интернет,
используйте SSH-туннель:

```powershell
ssh -L 3000:127.0.0.1:3000 root@SERVER_IP
```

После этого откройте:

```text
http://localhost:3000
```

Для QR-сканера аналогично:

```powershell
ssh -L 8080:127.0.0.1:8080 root@SERVER_IP
```

После туннеля откройте:

```text
http://localhost:8080/scanner
```

## 6. Резервная копия

На сервере:

```bash
cd /opt/spring-code-1
bash deploy/backup-postgres.sh
```

Файлы резервных копий сохраняются в:

```text
/opt/spring-code-1/backups
```

## 7. Восстановление

Скопируйте файл резервной копии на сервер и выполните:

```bash
cd /opt/spring-code-1
bash deploy/restore-postgres.sh backups/backup-file.sql.gz
```

Восстановление удаляет и создаёт заново базу приложения. Выполняйте его только
во время технического окна, когда бот можно временно остановить.
