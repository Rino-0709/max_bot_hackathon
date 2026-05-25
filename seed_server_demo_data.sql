BEGIN;

DELETE FROM entry_events
WHERE pass_request_id IN (
    SELECT id FROM pass_requests WHERE request_number LIKE 'PASS-DEMO-%'
);

DELETE FROM request_events
WHERE pass_request_id IN (
    SELECT id FROM pass_requests WHERE request_number LIKE 'PASS-DEMO-%'
);

DELETE FROM pass_requests
WHERE request_number LIKE 'PASS-DEMO-%';

DELETE FROM users
WHERE max_user_id BETWEEN 900000001 AND 900000180;

WITH active_zones AS (
    SELECT id, row_number() OVER (ORDER BY sort_order, id) AS rn
    FROM zones
    WHERE is_active = 1
),
demo_users AS (
    SELECT
        gs AS n,
        (900000000 + gs)::bigint AS max_user_id,
        'Тестовый гость ' || gs AS display_name
    FROM generate_series(1, 180) AS gs
),
inserted_users AS (
    INSERT INTO users (max_user_id, display_name, role, created_at)
    SELECT max_user_id, display_name, 'initiator', now()::text
    FROM demo_users
    RETURNING id, max_user_id
),
numbered_users AS (
    SELECT
        id,
        max_user_id,
        row_number() OVER (ORDER BY max_user_id) AS n
    FROM inserted_users
),
seed_rows AS (
    SELECT
        u.n,
        'PASS-DEMO-' || lpad(u.n::text, 4, '0') AS request_number,
        u.id AS user_id,
        CASE
            WHEN u.n <= 20 THEN 'pending_review'
            WHEN u.n <= 40 THEN 'clarification_requested'
            WHEN u.n <= 60 THEN 'approved'
            WHEN u.n <= 80 THEN 'passed'
            WHEN u.n <= 100 THEN 'rejected'
            WHEN u.n <= 120 THEN 'expired'
            WHEN u.n <= 140 THEN 'no_show'
            WHEN u.n <= 160 THEN 'cancelled_by_initiator'
            ELSE 'closed'
        END AS status,
        CASE
            WHEN u.n <= 100 THEN current_date
            WHEN u.n <= 120 THEN current_date - 1
            WHEN u.n <= 140 THEN current_date - 1
            ELSE current_date
        END AS visit_date,
        (ARRAY['09:00','10:00','11:00','12:00','13:00','14:00','15:00','16:00','17:00'])[((u.n - 1) % 9) + 1] AS visit_time,
        z.id AS zone_id,
        CASE
            WHEN u.n <= 20 THEN NULL
            WHEN u.n <= 40 THEN 'Уточните цель визита и контактное лицо.'
            WHEN u.n <= 60 THEN 'Одобрено для входа через главный пост.'
            WHEN u.n <= 80 THEN 'Проход подтвержден.'
            WHEN u.n <= 100 THEN (ARRAY[
                'Недостаточно данных',
                'Неверная дата визита',
                'Неверно указана зона',
                'Цель визита не подтверждена'
            ])[((u.n - 81) % 4) + 1]
            WHEN u.n <= 120 THEN 'Дата визита прошла без уточнения.'
            WHEN u.n <= 140 THEN 'Гость не пришел в день визита.'
            WHEN u.n <= 160 THEN 'Заявка отменена инициатором.'
            ELSE 'Заявка закрыта после проверки.'
        END AS public_comment
    FROM numbered_users u
    JOIN LATERAL (
        SELECT id
        FROM active_zones
        WHERE rn = ((u.n - 1) % (SELECT COUNT(*) FROM active_zones)) + 1
    ) z ON true
)
INSERT INTO pass_requests (
    request_number,
    user_id,
    full_name,
    visit_date,
    visit_time,
    zone_id,
    visit_purpose,
    status,
    public_comment,
    created_at,
    updated_at,
    closed_at
)
SELECT
    request_number,
    user_id,
    'Демо Фамилия ' || n || ' Имя ' || n || ' Отчество ' || n,
    visit_date::text,
    visit_time,
    zone_id,
    (ARRAY[
        'Консультация по пропускному режиму',
        'Встреча с сотрудником кафедры',
        'Передача документов',
        'Посещение лаборатории',
        'Собеседование по проекту'
    ])[((n - 1) % 5) + 1],
    status,
    public_comment,
    (now() - ((180 - n) || ' minutes')::interval)::text,
    now()::text,
    CASE
        WHEN status IN ('rejected', 'expired', 'no_show', 'cancelled_by_initiator', 'closed') THEN now()::text
        ELSE NULL
    END
FROM seed_rows;

INSERT INTO request_events (pass_request_id, actor_user_id, event_code, public_message, created_at)
SELECT
    pr.id,
    pr.user_id,
    'created',
    'Демо-заявка создана для проверки интерфейса.',
    pr.created_at
FROM pass_requests pr
WHERE pr.request_number LIKE 'PASS-DEMO-%';

INSERT INTO request_events (pass_request_id, actor_user_id, event_code, public_message, created_at)
SELECT
    pr.id,
    pr.user_id,
    pr.status,
    'Демо-статус: ' || pr.status,
    pr.updated_at
FROM pass_requests pr
WHERE pr.request_number LIKE 'PASS-DEMO-%'
  AND pr.status <> 'pending_review';

INSERT INTO entry_events (pass_request_id, actor_user_id, entry_type, created_at)
SELECT
    pr.id,
    pr.user_id,
    'first_entry',
    now()::text
FROM pass_requests pr
WHERE pr.request_number LIKE 'PASS-DEMO-%'
  AND pr.status = 'passed';

INSERT INTO entry_events (pass_request_id, actor_user_id, entry_type, created_at)
SELECT
    pr.id,
    pr.user_id,
    're_entry',
    (now() + interval '10 minutes')::text
FROM pass_requests pr
WHERE pr.request_number LIKE 'PASS-DEMO-%'
  AND pr.status = 'passed'
  AND right(pr.request_number, 1) IN ('0','2','4','6','8');

COMMIT;
