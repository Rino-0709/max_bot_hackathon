package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

func (app *App) showRequestCard(ctx context.Context, bctx BotContext, viewer UserRow, req RequestRow) error {
	rows := [][]Button{}
	if req.UserID == viewer.ID && (req.Status == "pending_review" || req.Status == "clarification_requested") {
		rows = append(rows, []Button{btn("Отменить заявку", fmt.Sprintf("request:cancel:%d", req.ID), "negative")})
	}
	if req.UserID == viewer.ID && req.Status == "clarification_requested" {
		rows = append(rows, []Button{btn("Ответить на уточнение", fmt.Sprintf("request:answer_clarification:%d", req.ID), "positive")})
	}
	rows = append(rows, []Button{btn("История", fmt.Sprintf("request:history:%d", req.ID), ""), btn("Проходы", fmt.Sprintf("request:entries:%d", req.ID), "")})

	if app.isAdmin(viewer) {
		if req.Status == "pending_review" {
			rows = append(rows, []Button{btn("Одобрить", fmt.Sprintf("admin:approve:%d", req.ID), "positive"), btn("Отклонить", fmt.Sprintf("admin:reject:%d", req.ID), "negative")})
			rows = append(rows, []Button{btn("Запросить уточнение", fmt.Sprintf("admin:clarify:%d", req.ID), "")})
		}
		if req.Status == "approved" || req.Status == "passed" {
			rows = append(rows, []Button{btn("Подтвердить проход", fmt.Sprintf("admin:entry:%d", req.ID), "positive")})
		}
		if contains([]string{"approved", "rejected", "passed", "no_show", "expired"}, req.Status) {
			rows = append(rows, []Button{btn("Закрыть", fmt.Sprintf("admin:close:%d", req.ID), "")})
		}
	}
	rows = append(rows, []Button{btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, app.requestCardText(req), rows)
}

func (app *App) requestCardText(req RequestRow) string {
	lines := []string{
		"Заявка: " + req.RequestNumber,
		"Статус: " + statusLabel(req.Status),
		"ФИО: " + req.FullName,
		"Дата: " + formatDate(req.VisitDate),
		"Время: " + req.VisitTime,
		"Корпус/зона: " + app.requestZone(req),
		"Цель: " + req.VisitPurpose,
		fmt.Sprintf("Проходы: %d", app.entryCount(req.ID)),
	}
	if req.PublicComment.Valid && req.PublicComment.String != "" {
		if req.Status != "clarification_requested" {
			lines = append(lines, "Комментарий: "+req.PublicComment.String)
		}
	}
	lines = append(lines, app.clarificationSummaryLines(req.ID)...)
	return strings.Join(lines, "\n")
}

func (app *App) clarificationSummaryLines(requestID int64) []string {
	// Показываем последний вопрос и ответ именно на него. Если уточнений было
	// несколько, старый ответ не должен выглядеть как ответ на новый вопрос.
	var question, questionAt string
	err := app.queryRow(`
		SELECT COALESCE(public_message, ''), created_at
		FROM request_events
		WHERE pass_request_id = ? AND event_code = 'clarification_requested'
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, requestID).Scan(&question, &questionAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		log.Printf("load clarification question for request %d: %v", requestID, err)
		return nil
	}

	lines := []string{
		"",
		"Уточнение:",
		"Вопрос: " + cleanEventText(question, "Запрошено уточнение:"),
	}

	var answer sql.NullString
	err = app.queryRow(`
		SELECT public_message
		FROM request_events
		WHERE pass_request_id = ?
			AND event_code = 'clarification_answered'
			AND created_at >= ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, requestID, questionAt).Scan(&answer)
	if err == nil && answer.Valid && strings.TrimSpace(answer.String) != "" {
		lines = append(lines, "Ответ: "+cleanEventText(answer.String, "Ответ инициатора:"))
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("load clarification answer for request %d: %v", requestID, err)
	}
	return lines
}

func cleanEventText(value, prefix string) string {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, prefix)
	return strings.TrimSpace(text)
}

func (app *App) showMyRequests(ctx context.Context, bctx BotContext, user UserRow) error {
	if err := app.expireOldRequests(ctx); err != nil {
		log.Printf("expire old requests: %v", err)
	}
	rows, err := app.query(`
		SELECT pr.id, pr.request_number, pr.user_id, pr.full_name, pr.visit_date, pr.visit_time,
			pr.zone_id, pr.custom_zone_text, pr.visit_purpose, pr.status, pr.public_comment,
			u.display_name, u.max_user_id, z.short_name, z.address, pr.created_at, pr.updated_at
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE pr.user_id = ?
		ORDER BY pr.created_at DESC
		LIMIT 10
	`, user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		req, err := app.scanRequest(rows)
		if err != nil {
			return err
		}
		found = true
		text := fmt.Sprintf("%s - %s\n%s, %s\n%s\nПроходы: %d", req.RequestNumber, statusLabel(req.Status), formatDate(req.VisitDate), req.VisitTime, app.requestZone(*req), app.entryCount(req.ID))
		if err := app.reply(ctx, bctx, text, [][]Button{{btn("Открыть", fmt.Sprintf("request:open:%d", req.ID), "")}}); err != nil {
			return err
		}
		bctx.CallbackID = ""
	}
	if !found {
		return app.reply(ctx, bctx, "У вас пока нет заявок.", [][]Button{
			{btn("Создать пропуск", "draft:start", "positive")},
			{btn("Назад", "menu", ""), btn("Главное меню", "menu", "")},
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return app.reply(ctx, bctx, "Показаны последние 10 заявок.", [][]Button{
		{btn("Назад", "menu", ""), btn("Главное меню", "menu", "")},
	})
}

func (app *App) showMyEntries(ctx context.Context, bctx BotContext, user UserRow) error {
	rows, err := app.query(`
		SELECT pr.request_number, pr.visit_date, COALESCE(z.short_name, pr.custom_zone_text, 'Не указано'), ee.entry_type, ee.created_at
		FROM entry_events ee
		JOIN pass_requests pr ON pr.id = ee.pass_request_id
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE pr.user_id = ?
		ORDER BY ee.created_at DESC
		LIMIT 20
	`, user.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var number, date, zone, entryType, created string
		if err := rows.Scan(&number, &date, &zone, &entryType, &created); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s - %s, %s\n%s: %s", number, formatDate(date), zone, entryLabel(entryType), formatDateTime(created)))
	}
	if len(lines) == 0 {
		return app.reply(ctx, bctx, "Проходов пока нет.", app.mainMenu(user))
	}
	return app.reply(ctx, bctx, strings.Join(lines, "\n\n"), app.mainMenu(user))
}

func (app *App) showHistory(ctx context.Context, bctx BotContext, requestID int64) error {
	rows, err := app.query(`
		SELECT event_code, public_message, created_at
		FROM request_events
		WHERE pass_request_id = ?
		ORDER BY created_at ASC
	`, requestID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var code string
		var message sql.NullString
		var created string
		if err := rows.Scan(&code, &message, &created); err != nil {
			return err
		}
		text := code
		if message.Valid && message.String != "" {
			text = message.String
		}
		lines = append(lines, fmt.Sprintf("%s - %s", formatDateTime(created), text))
	}
	if len(lines) == 0 {
		lines = []string{"История пока пустая."}
	}
	return app.reply(ctx, bctx, strings.Join(lines, "\n"), mainMenuRows())
}

func (app *App) showEntries(ctx context.Context, bctx BotContext, requestID int64) error {
	rows, err := app.query(`
		SELECT entry_type, created_at
		FROM entry_events
		WHERE pass_request_id = ?
		ORDER BY created_at ASC
	`, requestID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var entryType, created string
		if err := rows.Scan(&entryType, &created); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s - %s", formatDateTime(created), entryLabel(entryType)))
	}
	if len(lines) == 0 {
		lines = []string{"Проходов по заявке пока нет."}
	}
	return app.reply(ctx, bctx, strings.Join(lines, "\n"), mainMenuRows())
}
