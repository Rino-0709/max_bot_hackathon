SET client_encoding TO 'UTF8';

INSERT INTO users (max_user_id, display_name, role, created_at)
VALUES
    (910001, U&'\0414\0435\043c\043e\0020\0418\043d\0438\0446\0438\0430\0442\043e\0440', 'initiator', now()::text),
    (910002, U&'\0414\0435\043c\043e\0020\0410\0434\043c\0438\043d\0438\0441\0442\0440\0430\0442\043e\0440', 'admin', now()::text),
    (910003, U&'\0414\0435\043c\043e\0020\0422\0435\0445\0430\0434\043c\0438\043d', 'tech_admin', now()::text)
ON CONFLICT(max_user_id) DO UPDATE SET
    display_name = excluded.display_name,
    role = excluded.role;

INSERT INTO consents (user_id, document_version, scope, accepted_at)
SELECT id, 'hackathon-2026-05-24', 'profile_and_pass_requests', now()::text
FROM users
WHERE max_user_id IN (910001, 910002, 910003)
ON CONFLICT(user_id, document_version, scope) DO NOTHING;

WITH demo AS (
    SELECT
        (SELECT id FROM users WHERE max_user_id = 910001) AS initiator_id,
        (SELECT id FROM users WHERE max_user_id = 910002) AS admin_id,
        (SELECT id FROM zones WHERE code = 'vernadskogo-78') AS zone_78,
        (SELECT id FROM zones WHERE code = 'vernadskogo-86') AS zone_86
),
upsert_requests AS (
    INSERT INTO pass_requests (
        request_number, user_id, full_name, visit_date, visit_time, zone_id,
        visit_purpose, status, created_at, updated_at
    )
    SELECT 'PASS-DEMO-PEND1', initiator_id, U&'\0418\0432\0430\043d\043e\0432\0020\0418\0432\0430\043d\0020\0418\0432\0430\043d\043e\0432\0438\0447', current_date::text, '23:59', zone_78,
           U&'\0414\0435\043c\043e\003a\0020\0437\0430\044f\0432\043a\0430\0020\043d\0430\0020\0440\0430\0441\0441\043c\043e\0442\0440\0435\043d\0438\0438', 'pending_review', now()::text, now()::text
    FROM demo
    UNION ALL
    SELECT 'PASS-DEMO-APPR1', initiator_id, U&'\041f\0435\0442\0440\043e\0432\0020\041f\0435\0442\0440\0020\041f\0435\0442\0440\043e\0432\0438\0447', current_date::text, '23:59', zone_86,
           U&'\0414\0435\043c\043e\003a\0020\0430\043a\0442\0438\0432\043d\0430\044f\0020\043e\0434\043e\0431\0440\0435\043d\043d\0430\044f\0020\0437\0430\044f\0432\043a\0430', 'approved', now()::text, now()::text
    FROM demo
    UNION ALL
    SELECT 'PASS-DEMO-PASS1', initiator_id, U&'\0421\0438\0434\043e\0440\043e\0432\0020\0421\0435\043c\0435\043d\0020\0421\0435\0440\0433\0435\0435\0432\0438\0447', current_date::text, '23:59', zone_78,
           U&'\0414\0435\043c\043e\003a\0020\0437\0430\044f\0432\043a\0430\0020\0441\0020\043f\0440\043e\0445\043e\0434\043e\043c', 'passed', now()::text, now()::text
    FROM demo
    ON CONFLICT(request_number) DO UPDATE SET
        full_name = excluded.full_name,
        visit_date = excluded.visit_date,
        visit_time = excluded.visit_time,
        zone_id = excluded.zone_id,
        visit_purpose = excluded.visit_purpose,
        status = excluded.status,
        updated_at = now()::text
    RETURNING id, request_number
)
INSERT INTO request_events (pass_request_id, actor_user_id, event_code, public_message, created_at)
SELECT id, (SELECT id FROM users WHERE max_user_id = 910003), 'demo_seeded', U&'\0414\0435\043c\043e\002d\0434\0430\043d\043d\044b\0435\0020\0434\043e\0431\0430\0432\043b\0435\043d\044b\0020\0432\0020\0050\006f\0073\0074\0067\0072\0065\0053\0051\004c\002e', now()::text
FROM upsert_requests;

INSERT INTO entry_events (pass_request_id, actor_user_id, entry_type, created_at)
SELECT pr.id, u.id, 'first_entry', now()::text
FROM pass_requests pr
JOIN users u ON u.max_user_id = 910002
WHERE pr.request_number = 'PASS-DEMO-PASS1'
  AND NOT EXISTS (
      SELECT 1 FROM entry_events ee
      WHERE ee.pass_request_id = pr.id AND ee.entry_type = 'first_entry'
  );
