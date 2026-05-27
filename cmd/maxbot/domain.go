package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (app *App) approveRequest(ctx context.Context, bctx BotContext, actor UserRow, requestID int64, comment string) error {
	message := "Заявка одобрена."
	if comment != "" {
		message += " Комментарий: " + comment
	}
	if err := app.updateRequestStatus(requestID, "approved", actor, message, comment); err != nil {
		return err
	}
	notification := "Ваша заявка одобрена.\nНомер: " + app.requestNumber(requestID)
	if comment != "" {
		notification += "\nКомментарий: " + comment
	}
	app.notifyOwner(ctx, requestID, notification)
	return app.reply(ctx, bctx, "Заявка одобрена.", adminBackRows())
}

func (app *App) updateRequestStatus(requestID int64, status string, actor UserRow, message string, publicComment string) error {
	var current string
	if err := app.queryRow(`SELECT status FROM pass_requests WHERE id = ?`, requestID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("Заявка не найдена.")
		}
		return err
	}
	if !allowedStatusTransition(current, status) {
		return fmt.Errorf("Недопустимый переход статуса: %s -> %s.", statusLabel(current), statusLabel(status))
	}

	now := nowISO()
	_, err := app.exec(`
		UPDATE pass_requests
		SET status = ?,
			public_comment = NULLIF(?, ''),
			updated_at = ?,
			closed_at = CASE WHEN ? = 'closed' THEN ? ELSE closed_at END
		WHERE id = ?
	`, status, publicComment, now, status, now, requestID)
	if err != nil {
		return err
	}
	app.requestEvent(requestID, &actor.ID, status, message)
	app.audit(actor.MaxUserID, "request_"+status, "pass_request", requestID, nil)
	return nil
}

func allowedStatusTransition(from, to string) bool {
	if from == to {
		return true
	}
	// Статусы специально оформлены как белый список переходов. Так проще
	// объяснить поведение на защите и сложнее случайно "перепрыгнуть" аудит.
	allowed := map[string][]string{
		"pending_review":             {"approved", "rejected", "clarification_requested", "cancelled_by_initiator", "expired", "data_erasure_requested"},
		"clarification_requested":    {"pending_review", "cancelled_by_initiator", "expired", "data_erasure_requested"},
		"approved":                   {"passed", "no_show", "closed", "data_erasure_requested"},
		"passed":                     {"closed"},
		"rejected":                   {"closed"},
		"expired":                    {"closed"},
		"no_show":                    {"closed"},
		"cancelled_by_initiator":     {"closed"},
		"cancelled_by_administrator": {"closed"},
		"data_erasure_requested":     {"closed"},
	}
	return contains(allowed[from], to)
}

func cleanAdminComment(text string) string {
	comment := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if len([]rune(comment)) > 240 {
		comment = string([]rune(comment)[:240])
	}
	return comment
}

func (app *App) registerEntry(requestID int64, actor UserRow) (string, error) {
	req, err := app.requestByID(requestID)
	if err != nil {
		return "", err
	}
	if req == nil {
		return "", errors.New("Заявка не найдена.")
	}
	if req.Status != "approved" && req.Status != "passed" {
		return "", errors.New("Проход можно подтвердить только для одобренной заявки.")
	}
	if req.VisitDate != todayMoscow() {
		return "", errors.New("Проход можно подтвердить только в дату визита.")
	}
	count := app.entryCount(requestID)
	entryType := "first_entry"
	message := "Администратор подтвердил первый проход."
	if count > 0 {
		// Пропуск действует весь день: повторный вход не создаёт новую заявку,
		// а фиксируется отдельным событием прохода в той же карточке.
		entryType = "re_entry"
		message = "Администратор подтвердил повторный проход."
	}
	if entryType == "first_entry" {
		if !allowedStatusTransition(req.Status, "passed") {
			return "", fmt.Errorf("Недопустимый переход статуса: %s -> %s.", statusLabel(req.Status), statusLabel("passed"))
		}
	}
	if _, err := app.exec(`INSERT INTO entry_events (pass_request_id, actor_user_id, entry_type, created_at) VALUES (?, ?, ?, ?)`, requestID, actor.ID, entryType, nowISO()); err != nil {
		return "", err
	}
	if entryType == "first_entry" {
		if _, err := app.exec(`UPDATE pass_requests SET status = 'passed', updated_at = ? WHERE id = ?`, nowISO(), requestID); err != nil {
			return "", err
		}
	}
	app.requestEvent(requestID, &actor.ID, entryType, message)
	app.audit(actor.MaxUserID, entryType, "pass_request", requestID, nil)
	return entryType, nil
}

func (app *App) entryCount(requestID int64) int {
	var count int
	_ = app.queryRow(`SELECT COUNT(*) FROM entry_events WHERE pass_request_id = ?`, requestID).Scan(&count)
	return count
}

func (app *App) expireOldRequests(ctx context.Context) error {
	// Просрочки считаются лениво при открытии списков. Для MVP это проще
	// фонового планировщика, а в проде эту функцию можно вынести в cron/job.
	rows, err := app.query(`
		SELECT pr.id, pr.request_number, pr.status, u.max_user_id
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		WHERE pr.visit_date < ? AND pr.status IN ('pending_review', 'approved', 'clarification_requested')
	`, todayMoscow())
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id, maxUserID int64
		var number, status string
		if err := rows.Scan(&id, &number, &status, &maxUserID); err != nil {
			return err
		}
		next := "expired"
		message := "Дата визита прошла, заявка устарела."
		notify := "Заявка " + number + ": дата визита прошла, заявка устарела."
		if status == "approved" {
			message = "Дата визита прошла, проход не был подтверждён."
			notify = "Заявка " + number + ": дата визита прошла, заявка устарела, проход не был подтверждён."
		}
		if status == "clarification_requested" {
			message = "Дата визита прошла, уточнение не было предоставлено."
			notify = "Заявка " + number + ": заявка устарела без уточнения."
		}
		if _, err := app.exec(`UPDATE pass_requests SET status = ?, updated_at = ? WHERE id = ?`, next, nowISO(), id); err != nil {
			return err
		}
		app.requestEvent(id, nil, next, message)
		_ = app.sendUserNotification(ctx, maxUserID, notify)
	}
	return rows.Err()
}

func (app *App) notifyOwner(ctx context.Context, requestID int64, text string) {
	app.notifyOwnerWithRows(ctx, requestID, text, nil)
}

func (app *App) notifyOwnerWithRows(ctx context.Context, requestID int64, text string, rows [][]Button) {
	var maxUserID int64
	if err := app.queryRow(`
		SELECT u.max_user_id
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		WHERE pr.id = ?
	`, requestID).Scan(&maxUserID); err == nil {
		_ = app.sendUserNotificationWithRows(ctx, maxUserID, text, rows)
	}
}

func (app *App) sendUserNotification(ctx context.Context, maxUserID int64, text string) error {
	return app.sendUserNotificationWithRows(ctx, maxUserID, text, nil)
}

func (app *App) sendUserNotificationWithRows(ctx context.Context, maxUserID int64, text string, rows [][]Button) error {
	if app.api == nil {
		app.testReplies = append(app.testReplies, TestReply{Text: text, Rows: rows})
		return nil
	}
	return app.api.SendToUser(ctx, maxUserID, text, rows)
}

func (app *App) requestNumber(requestID int64) string {
	var number string
	_ = app.queryRow(`SELECT request_number FROM pass_requests WHERE id = ?`, requestID).Scan(&number)
	return number
}

func (app *App) requestEvent(requestID int64, actorUserID *int64, code, message string) {
	_, _ = app.exec(`
		INSERT INTO request_events (pass_request_id, actor_user_id, event_code, public_message, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, requestID, actorUserID, code, message, nowISO())
}

func (app *App) audit(actorMaxUserID int64, action, entityType string, entityID int64, metadata map[string]string) {
	var payload interface{}
	if metadata != nil {
		data, _ := json.Marshal(metadata)
		payload = string(data)
	}
	_, _ = app.exec(`
		INSERT INTO audit_log (actor_max_user_id, action, entity_type, entity_id, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, actorMaxUserID, action, entityType, entityID, payload, nowISO())
}

func (app *App) setSession(maxUserID int64, state string, data map[string]string) {
	if data == nil {
		data = map[string]string{}
	}
	raw, _ := json.Marshal(data)
	_, _ = app.exec(`
		INSERT INTO sessions (max_user_id, state, data_json, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(max_user_id) DO UPDATE SET state = excluded.state, data_json = excluded.data_json, updated_at = excluded.updated_at
	`, maxUserID, state, string(raw), nowISO())
}

func (app *App) getSession(maxUserID int64) (Session, bool) {
	var state string
	var raw sql.NullString
	err := app.queryRow(`SELECT state, data_json FROM sessions WHERE max_user_id = ?`, maxUserID).Scan(&state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false
	}
	if err != nil {
		return Session{}, false
	}
	data := map[string]string{}
	if raw.Valid && raw.String != "" {
		_ = json.Unmarshal([]byte(raw.String), &data)
	}
	return Session{State: state, Data: data}, true
}

func (app *App) isEditSession(maxUserID int64, state string) bool {
	session, ok := app.getSession(maxUserID)
	return ok && session.State == state && session.Data["mode"] == "edit"
}

func (app *App) clearSession(maxUserID int64) {
	_, _ = app.exec(`DELETE FROM sessions WHERE max_user_id = ?`, maxUserID)
}
