package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strings"
)

// initDB создает таблицы и мягко докатывает схему при старте.
func (app *App) initDB() error {
	// Миграции здесь намеренно простые и идемпотентные: на хакатонном стенде
	// бот должен поднимать пустую базу сам, без отдельной команды DBA.
	_, err := app.exec(`
		CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			max_user_id BIGINT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'initiator',
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS consents (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL REFERENCES users(id),
			document_version TEXT NOT NULL,
			scope TEXT NOT NULL,
			accepted_at TEXT NOT NULL,
			UNIQUE(user_id, document_version, scope)
		);

		CREATE TABLE IF NOT EXISTS zones (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL UNIQUE,
			short_name TEXT NOT NULL,
			address TEXT NOT NULL,
			is_active INTEGER NOT NULL DEFAULT 1,
			sort_order INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS draft_requests (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL UNIQUE REFERENCES users(id),
			full_name TEXT,
			visit_date TEXT,
			visit_time TEXT,
			zone_id BIGINT REFERENCES zones(id),
			custom_zone_text TEXT,
			visit_purpose TEXT,
			extra_fields_json TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS pass_requests (
			id BIGSERIAL PRIMARY KEY,
			request_number TEXT NOT NULL UNIQUE,
			user_id BIGINT NOT NULL REFERENCES users(id),
			full_name TEXT NOT NULL,
			visit_date TEXT NOT NULL,
			visit_time TEXT NOT NULL,
			zone_id BIGINT REFERENCES zones(id),
			custom_zone_text TEXT,
			visit_purpose TEXT NOT NULL,
			extra_fields_json TEXT,
			status TEXT NOT NULL,
			admin_reason_code TEXT,
			public_comment TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			closed_at TEXT
		);

		CREATE TABLE IF NOT EXISTS request_events (
			id BIGSERIAL PRIMARY KEY,
			pass_request_id BIGINT NOT NULL REFERENCES pass_requests(id),
			actor_user_id BIGINT REFERENCES users(id),
			event_code TEXT NOT NULL,
			public_message TEXT,
			metadata_json TEXT,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS entry_events (
			id BIGSERIAL PRIMARY KEY,
			pass_request_id BIGINT NOT NULL REFERENCES pass_requests(id),
			actor_user_id BIGINT NOT NULL REFERENCES users(id),
			entry_type TEXT NOT NULL,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS extra_field_definitions (
			id BIGSERIAL PRIMARY KEY,
			label TEXT NOT NULL UNIQUE,
			is_active INTEGER NOT NULL DEFAULT 1,
			sort_order INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS sessions (
			max_user_id BIGINT PRIMARY KEY,
			state TEXT NOT NULL,
			data_json TEXT,
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS audit_log (
			id BIGSERIAL PRIMARY KEY,
			actor_max_user_id BIGINT,
			action TEXT NOT NULL,
			entity_type TEXT,
			entity_id BIGINT,
			metadata_json TEXT,
			created_at TEXT NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_pass_requests_user_status ON pass_requests(user_id, status);
		CREATE INDEX IF NOT EXISTS idx_pass_requests_status_date ON pass_requests(status, visit_date);
		CREATE INDEX IF NOT EXISTS idx_entry_events_request ON entry_events(pass_request_id);
		CREATE INDEX IF NOT EXISTS idx_audit_log_actor_created ON audit_log(actor_max_user_id, created_at DESC);

		ALTER TABLE draft_requests ADD COLUMN IF NOT EXISTS extra_fields_json TEXT;
		ALTER TABLE pass_requests ADD COLUMN IF NOT EXISTS extra_fields_json TEXT;
	`)
	if err != nil {
		return err
	}

	for i, zone := range initialZones {
		if _, err := app.exec(`
			INSERT INTO zones (code, short_name, address, sort_order)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(code) DO NOTHING
		`, zone.Code, zone.ShortName, zone.Address, i+1); err != nil {
			return err
		}
	}
	return nil
}

// upsertUser сохраняет пользователя MAX и обновляет его отображаемое имя.
func (app *App) upsertUser(maxUser MaxUser) (UserRow, error) {
	role := roleInitiator
	if app.cfg.AdminIDs[maxUser.UserID] {
		role = roleAdmin
	}
	if app.cfg.TechAdminIDs[maxUser.UserID] {
		role = roleTechAdmin
	}
	name := displayName(maxUser)
	now := nowISO()

	_, err := app.exec(`
		INSERT INTO users (max_user_id, display_name, role, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(max_user_id) DO UPDATE SET
			display_name = excluded.display_name,
			role = CASE
				WHEN users.role = 'tech_admin' THEN users.role
				WHEN excluded.role = 'tech_admin' THEN excluded.role
				WHEN users.role = 'admin' THEN users.role
				WHEN excluded.role = 'admin' THEN excluded.role
				ELSE users.role
			END
	`, maxUser.UserID, name, role, now)
	if err != nil {
		return UserRow{}, err
	}

	var user UserRow
	err = app.queryRow(`SELECT id, max_user_id, display_name, role FROM users WHERE max_user_id = ?`, maxUser.UserID).
		Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role)
	return user, err
}

// userByMaxID ищет локального пользователя по MAX user id.
func (app *App) userByMaxID(maxUserID int64) (*UserRow, error) {
	var user UserRow
	err := app.queryRow(`SELECT id, max_user_id, display_name, role FROM users WHERE max_user_id = ?`, maxUserID).
		Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// hasConsent проверяет наличие актуального согласия пользователя.
func (app *App) hasConsent(userID int64) bool {
	var ok int
	_ = app.queryRow(`SELECT 1 FROM consents WHERE user_id = ? AND document_version = ? AND scope = 'profile_and_pass_requests'`, userID, app.cfg.PolicyVersion).Scan(&ok)
	return ok == 1
}

// isAdmin проверяет, доступны ли пользователю админские функции.
func (app *App) isAdmin(user UserRow) bool {
	return user.Role == roleAdmin || user.Role == roleTechAdmin
}

// isTechAdmin проверяет, доступны ли пользователю техадминские функции.
func (app *App) isTechAdmin(user UserRow) bool {
	return user.Role == roleTechAdmin
}

// setRole назначает пользователю новую роль.
func (app *App) setRole(maxUserID int64, role string) error {
	_, err := app.exec(`UPDATE users SET role = ? WHERE max_user_id = ?`, role, maxUserID)
	return err
}

// getDraft возвращает текущий черновик анкеты пользователя.
func (app *App) getDraft(userID int64) (*DraftRow, error) {
	row := app.queryRow(`
		SELECT id, user_id, full_name, visit_date, visit_time, zone_id, custom_zone_text, visit_purpose, extra_fields_json
		FROM draft_requests WHERE user_id = ?
	`, userID)
	var draft DraftRow
	err := row.Scan(&draft.ID, &draft.UserID, &draft.FullName, &draft.VisitDate, &draft.VisitTime, &draft.ZoneID, &draft.CustomZoneText, &draft.VisitPurpose, &draft.ExtraFieldsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &draft, err
}

// ensureDraft создает пустой черновик, если пользователь только начал анкету.
func (app *App) ensureDraft(userID int64) error {
	_, err := app.exec(`
		INSERT INTO draft_requests (user_id, created_at, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO NOTHING
	`, userID, nowISO(), nowISO())
	return err
}

// updateDraft обновляет только те поля черновика, которые реально изменились.
func (app *App) updateDraft(userID int64, values map[string]interface{}) error {
	if len(values) == 0 {
		return nil
	}
	allowed := map[string]bool{
		"full_name": true, "visit_date": true, "visit_time": true, "zone_id": true, "custom_zone_text": true, "visit_purpose": true, "extra_fields_json": true,
	}
	set := make([]string, 0, len(values)+1)
	args := make([]interface{}, 0, len(values)+2)
	for key, value := range values {
		if !allowed[key] {
			continue
		}
		set = append(set, key+" = ?")
		args = append(args, value)
	}
	set = append(set, "updated_at = ?")
	args = append(args, nowISO(), userID)
	_, err := app.exec(`UPDATE draft_requests SET `+strings.Join(set, ", ")+` WHERE user_id = ?`, args...)
	return err
}

// deleteDraft удаляет черновик после отправки или отказа от анкеты.
func (app *App) deleteDraft(userID int64) {
	_, _ = app.exec(`DELETE FROM draft_requests WHERE user_id = ?`, userID)
}

// createPassRequest превращает заполненный черновик в заявку.
func (app *App) createPassRequest(user UserRow) (string, error) {
	draft, err := app.getDraft(user.ID)
	if err != nil {
		return "", err
	}
	if draft == nil || !draft.FullName.Valid || !draft.VisitDate.Valid || !draft.VisitTime.Valid || !draft.VisitPurpose.Valid || (!draft.ZoneID.Valid && !draft.CustomZoneText.Valid) {
		return "", errors.New("Черновик заполнен не полностью.")
	}
	if err := validateDate(draft.VisitDate.String); err != nil {
		return "", err
	}
	if errText := validateVisitDateTime(draft.VisitDate.String, draft.VisitTime.String); errText != "" {
		return "", errors.New(errText)
	}
	if field, ok, err := app.nextMissingExtraField(user.ID); err != nil {
		return "", err
	} else if ok {
		return "", fmt.Errorf("Заполните дополнительное поле: %s.", field.Label)
	}

	statuses := []string{"pending_review", "clarification_requested", "approved", "passed"}
	query := `
		SELECT request_number FROM pass_requests
		WHERE user_id = ?
			AND visit_date = ?
			AND COALESCE(zone_id, 0) = COALESCE(?, 0)
			AND COALESCE(custom_zone_text, '') = COALESCE(?, '')
			AND status IN (` + placeholders(len(statuses)) + `)
		LIMIT 1
	`
	args := []interface{}{user.ID, draft.VisitDate.String, nullableIntArg(draft.ZoneID), nullableStringArg(draft.CustomZoneText)}
	for _, status := range statuses {
		args = append(args, status)
	}
	var duplicate string
	err = app.queryRow(query, args...).Scan(&duplicate)
	if err == nil {
		return "", fmt.Errorf("Похожая активная заявка уже есть: %s.", duplicate)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	number := app.makeRequestNumber()
	for app.requestNumberExists(number) {
		number = app.makeRequestNumber()
	}

	now := nowISO()
	var requestID int64
	err = app.queryRow(`
		INSERT INTO pass_requests (
			request_number, user_id, full_name, visit_date, visit_time, zone_id, custom_zone_text,
			visit_purpose, extra_fields_json, status, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_review', ?, ?)
		RETURNING id
	`, number, user.ID, draft.FullName.String, draft.VisitDate.String, draft.VisitTime.String, nullableIntArg(draft.ZoneID), nullableStringArg(draft.CustomZoneText), draft.VisitPurpose.String, nullableStringArg(draft.ExtraFieldsJSON), now, now).Scan(&requestID)
	if err != nil {
		return "", err
	}
	app.requestEvent(requestID, &user.ID, "created", "Заявка создана и отправлена на рассмотрение.")
	app.audit(user.MaxUserID, "pass_request_created", "pass_request", requestID, nil)
	app.deleteDraft(user.ID)
	return number, nil
}

// makeRequestNumber генерирует короткий читаемый номер заявки.
func (app *App) makeRequestNumber() string {
	alphabet := []rune("23456789ABCDEFGHJKLMNPQRSTUVWXYZ")
	var suffix strings.Builder
	for i := 0; i < 5; i++ {
		suffix.WriteRune(alphabet[rand.Intn(len(alphabet))])
	}
	return "PASS-" + strings.ReplaceAll(todayMoscow(), "-", "") + "-" + suffix.String()
}

// requestNumberExists проверяет, не занят ли сгенерированный номер.
func (app *App) requestNumberExists(number string) bool {
	var ok int
	_ = app.queryRow(`SELECT 1 FROM pass_requests WHERE request_number = ?`, number).Scan(&ok)
	return ok == 1
}

// requestByID загружает заявку по внутреннему id.
func (app *App) requestByID(id int64) (*RequestRow, error) {
	return app.scanRequest(app.queryRow(`
		SELECT pr.id, pr.request_number, pr.user_id, pr.full_name, pr.visit_date, pr.visit_time,
			pr.zone_id, pr.custom_zone_text, pr.visit_purpose, pr.extra_fields_json, pr.status, pr.public_comment,
			u.display_name, u.max_user_id, z.short_name, z.address, pr.created_at, pr.updated_at
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE pr.id = ?
	`, id))
}

// requestByNumber ищет заявку по полному номеру или последним символам.
func (app *App) requestByNumber(number string) (*RequestRow, error) {
	query := strings.TrimSpace(number)
	return app.scanRequest(app.queryRow(`
		SELECT pr.id, pr.request_number, pr.user_id, pr.full_name, pr.visit_date, pr.visit_time,
			pr.zone_id, pr.custom_zone_text, pr.visit_purpose, pr.extra_fields_json, pr.status, pr.public_comment,
			u.display_name, u.max_user_id, z.short_name, z.address, pr.created_at, pr.updated_at
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE UPPER(pr.request_number) = UPPER(?)
			OR (? = 5 AND SUBSTR(UPPER(pr.request_number), LENGTH(pr.request_number) - 4) = UPPER(?))
		ORDER BY pr.created_at DESC
	`, query, len([]rune(query)), query))
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

// scanRequest читает заявку из SQL-строки в RequestRow.
func (app *App) scanRequest(row rowScanner) (*RequestRow, error) {
	var req RequestRow
	err := row.Scan(&req.ID, &req.RequestNumber, &req.UserID, &req.FullName, &req.VisitDate, &req.VisitTime, &req.ZoneID, &req.CustomZoneText, &req.VisitPurpose, &req.ExtraFieldsJSON, &req.Status, &req.PublicComment, &req.DisplayName, &req.MaxUserID, &req.ZoneName, &req.ZoneAddress, &req.CreatedAt, &req.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}
