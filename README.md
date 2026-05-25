# Весенний_код_1

Go-бот для MAX: электронное бюро пропусков с оформлением разового гостевого пропуска.

## Что важно для жюри

- **Инновационность:** бот не просто собирает форму, а ведёт весь жизненный цикл пропуска: черновик, уточнения, статусы, проходы, аудит, экспорт и мониторинг.
- **Техническая реализация:** Go, PostgreSQL, Docker Compose, Prometheus, Grafana, сценарные автотесты и идемпотентная инициализация БД.
- **Практическая ценность:** решение закрывает реальную боль бюро пропусков: меньше ручных сообщений, меньше дублей, прозрачная история и быстрый поиск по номеру заявки.
- **UX:** почти все действия сделаны кнопками; свободный ввод остаётся только там, где без него нельзя: ФИО, цель, комментарии и ответы на уточнения.
- **Безопасность данных:** минимальный сбор данных, согласие с версией документа, отзыв согласия, роли, аудит и обезличивание заявок.

## Возможности

- согласие на минимальную обработку данных, экран политики данных и отзыв согласия;
- роли: инициатор, администратор, технический администратор;
- создание заявки только для себя;
- черновик заявки;
- выбор даты, времени и корпуса кнопками;
- список корпусов РТУ МИРЭА в Москве;
- защита от дублей на одну дату и одну зону;
- очередь заявок администратора;
- одобрение с необязательным комментарием, отклонение с причиной, запрос уточнения;
- подтверждение первого и повторного прохода администратором;
- Excel-экспорт заявок на сегодня: активные и закрытые на отдельных листах;
- история заявки и список проходов;
- управление зонами и выдача роли администратора техадмином;
- просмотр списка администраторов, аудит их действий и отзыв роли администратора.

## Архитектура

Код разделён по зонам ответственности:

- [main.go](cmd/maxbot/main.go) - запуск приложения;
- [config.go](cmd/maxbot/config.go) - `.env` и PostgreSQL;
- [max_api.go](cmd/maxbot/max_api.go) - клиент MAX API;
- [polling.go](cmd/maxbot/polling.go) - long polling, порядок событий и маршрутизация;
- [storage.go](cmd/maxbot/storage.go) - схема БД и хранилище;
- [request_views.go](cmd/maxbot/request_views.go) - карточки заявок, история и проходы;
- [admin.go](cmd/maxbot/admin.go) - сценарии администратора и Excel-экспорт;
- [tech_admin.go](cmd/maxbot/tech_admin.go) - техадмин, роли, зоны и аудит;
- [domain.go](cmd/maxbot/domain.go) - бизнес-правила и статусы;
- [observability.go](cmd/maxbot/observability.go) - healthcheck и Prometheus;
- [util.go](cmd/maxbot/util.go) - валидация и форматирование.

Подробное описание решений: [docs/architecture.md](docs/architecture.md).

## Требования

Нужен установленный Go. Проверка:

```bash
go version
```

## Настройка

Файл `.env`:

```env
BOT_TOKEN=your_max_bot_token
DB_DRIVER=postgres
DATABASE_URL=postgres://spring_code_bot:change_me_before_deploy@127.0.0.1:55432/spring_code_passes?sslmode=disable
DATA_DIR=./data
POLICY_VERSION=hackathon-2026-05-24
ADMIN_USER_IDS=
TECH_ADMIN_USER_IDS=254098701
ADMIN_BOOTSTRAP_CODE=Vesna2026
```

Роли читаются именно из `.env`, не из `.env.example`.

### База данных

Проект использует PostgreSQL. Укажите строку подключения:

```env
DB_DRIVER=postgres
DATABASE_URL=postgres://user:password@host:5432/database?sslmode=disable
```

При первом запуске бот создаёт таблицы и базовые зоны автоматически.

Для локальной разработки проще поднять PostgreSQL через Docker Compose:

```powershell
docker compose up -d postgres
```

Если бот запускается не в контейнере через `go run`, `DATABASE_URL` должен смотреть на локальный порт PostgreSQL:

```env
DATABASE_URL=postgres://spring_code_bot:change_me_before_deploy@127.0.0.1:55432/spring_code_passes?sslmode=disable
```

Демо-данные для ручной проверки загружаются в PostgreSQL из Docker Compose:

```powershell
.\seed_test_data.ps1
```

Данные для проверки пагинации:

```powershell
.\seed_pagination_data.ps1
```

## Запуск

```bash
go mod tidy
go run ./cmd/maxbot
```

После изменения `.env` бот нужно перезапустить.

### Docker Compose

Для локального стенда с ботом, PostgreSQL, Prometheus и Grafana:

```powershell
docker compose up -d --build
```

Сервисы:

- бот и метрики: `http://localhost:8080/metrics`;
- healthcheck бота: `http://localhost:8080/healthz`;
- Prometheus: `http://localhost:9090`;
- Grafana: `http://localhost:3000`.

Compose читает секреты и роли из `.env`, но строку подключения к PostgreSQL внутри контейнеров задаёт сам через сервис `postgres`. Перед деплоем обязательно поменяйте `POSTGRES_PASSWORD` и `GRAFANA_ADMIN_PASSWORD`.

Порты мониторинга по умолчанию привязаны к `127.0.0.1`, чтобы не открыть Grafana и Prometheus наружу случайно. Для внешнего доступа лучше поставить nginx с авторизацией или открыть доступ только через VPN.

Остановка:

```powershell
docker compose down
```

Остановка с удалением контейнерных данных:

```powershell
docker compose down -v
```

## Мониторинг

Бот отдаёт:

- `/healthz` и `/readyz` - проверка доступности процесса и PostgreSQL;
- `/metrics` - Prometheus-метрики.

Основные метрики:

- `maxbot_uptime_seconds` - время работы процесса;
- `maxbot_updates_total` - входящие события MAX по типам;
- `maxbot_update_errors_total` и `maxbot_poll_errors_total` - ошибки обработки и polling;
- `maxbot_replies_total` - ответы бота;
- `maxbot_pass_requests_total{status="..."}` - заявки по статусам;
- `maxbot_users_total`, `maxbot_entry_events_total`, `maxbot_audit_events_total`;
- `maxbot_db_open_connections`, `maxbot_db_in_use_connections`, `maxbot_db_idle_connections`.

Grafana автоматически подключает Prometheus и загружает dashboard `Весенний_код_1 overview`.

## Деплой на сервер

Подготовленные файлы для переезда лежат в [deploy](deploy/README.md).

Короткий путь:

```powershell
.\deploy\upload.ps1 -ServerHost "SERVER_IP" -User "root"
```

На сервере:

```bash
cd /opt/spring-code-1
cp .env.production.example .env
nano .env
docker compose up -d --build
bash deploy/check-server.sh
```

Grafana и Prometheus по умолчанию доступны только с самого сервера. Для безопасного просмотра Grafana с локального компьютера используйте SSH tunnel:

```powershell
ssh -L 3000:127.0.0.1:3000 root@SERVER_IP
```

## Тесты

Все автотесты и проверка сборки запускаются одним файлом:

```powershell
.\run_tests.ps1
```

Скрипт поднимает PostgreSQL через Docker Compose, при отсутствии локального `.env` создаёт его из `.env.example`, затем запускает тесты и сборку.

Сценарные тесты читаются как ручная проверка бота: `Command`, `Say`, `Click`, `ExpectText`, `ExpectButton`. Они покрывают основные пути инициатора, администратора и технического администратора.

Вручную то же самое:

```powershell
go test ./...
go test ./cmd/maxbot -run TestScenario -v
go build ./cmd/maxbot
```

## Команды

- `/start` - открыть главное меню;
- `/whoami` - показать MAX user id и текущую роль;
- `/admin <код>` - выдать текущему аккаунту роль администратора через bootstrap-код;
- `/reset_session` - сбросить текущий текстовый ввод.

Можно также отправить текст `Главное меню` или `Меню` - бот откроет главное меню без команды `/start`.

В текущем MAX Bot API основной надёжный вариант навигации - inline-кнопки в сообщениях. Постоянная Telegram-style клавиатура под полем ввода в этом MVP не используется.

## Данные и согласие

На первом запуске бот показывает дисклеймер, перечень обрабатываемых данных и версию документов. Пользователь может открыть экран `Политика данных` до согласия или позже из главного меню через `Политика и согласие`.

На экране политики пользователь может отозвать согласие. После подтверждения бот удаляет черновик, снимает согласие и обезличивает данные активных заявок, сохраняя минимальный технический аудит.

## Техадмин

В меню `Техадмин` доступны:

- `Обычные админы` - список обычных администраторов;
- `Тех админы` - список технических администраторов;
- карточка администратора - роль, MAX user id и количество действий в аудите;
- `Действия` - последние 20 действий выбранного администратора;
- `Забрать роль админа` - перевод обычного администратора обратно в инициаторы;
- `Добавить админа` - назначение роли по MAX user id внутри раздела `Обычные админы`.

Выдать роль можно только пользователю, который уже известен боту. Если пользователь ещё ни разу не писал `/start` или `/whoami`, бот покажет, что такой MAX user id не найден.

## Как проверить техадмина

1. В `.env` укажите свой MAX user id в `TECH_ADMIN_USER_IDS`.
2. Перезапустите бота.
3. Напишите боту `/whoami`.
4. В ответе должна быть роль `технический администратор`.
5. После `/start` и согласия в меню появится кнопка `Техадмин`.
