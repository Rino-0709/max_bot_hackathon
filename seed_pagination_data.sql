SET client_encoding TO 'UTF8';

WITH seed_users AS (
    INSERT INTO users (max_user_id, display_name, role, created_at)
    SELECT
        920000 + n,
        'Pagination User ' || lpad(n::text, 3, '0'),
        'initiator',
        now()::text
    FROM generate_series(1, 100) AS n
    ON CONFLICT(max_user_id) DO UPDATE SET
        display_name = excluded.display_name,
        role = excluded.role
    RETURNING id, max_user_id
),
consent_seed AS (
    INSERT INTO consents (user_id, document_version, scope, accepted_at)
    SELECT id, 'hackathon-2026-05-24', 'profile_and_pass_requests', now()::text
    FROM seed_users
    ON CONFLICT(user_id, document_version, scope) DO NOTHING
),
zone_pool AS (
    SELECT id, row_number() OVER (ORDER BY sort_order, id) AS rn
    FROM zones
    WHERE is_active = 1
),
numbered AS (
    SELECT
        u.id AS user_id,
        (u.max_user_id - 920000)::int AS n,
        zp.id AS zone_id
    FROM seed_users u
    JOIN zone_pool zp ON zp.rn = ((u.max_user_id - 920001) % (SELECT count(*) FROM zone_pool)) + 1
)
INSERT INTO pass_requests (
    request_number, user_id, full_name, visit_date, visit_time, zone_id,
    visit_purpose, status, created_at, updated_at
)
SELECT
    'PASS-PAGE-' || lpad(n::text, 3, '0'),
    user_id,
    'Pagination Test User ' || lpad(n::text, 3, '0'),
    current_date::text,
    CASE (n % 5)
        WHEN 0 THEN '09:00'
        WHEN 1 THEN '11:00'
        WHEN 2 THEN '13:00'
        WHEN 3 THEN '15:00'
        ELSE '17:00'
    END,
    zone_id,
    'Pagination test request #' || n,
    CASE
        WHEN n <= 55 THEN 'pending_review'
        WHEN n <= 85 THEN 'approved'
        ELSE 'passed'
    END,
    (now() - make_interval(mins => 100 - n))::text,
    now()::text
FROM numbered
ON CONFLICT(request_number) DO UPDATE SET
    visit_date = excluded.visit_date,
    visit_time = excluded.visit_time,
    zone_id = excluded.zone_id,
    visit_purpose = excluded.visit_purpose,
    status = excluded.status,
    updated_at = now()::text;
