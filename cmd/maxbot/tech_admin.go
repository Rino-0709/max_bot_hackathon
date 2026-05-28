package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// techMenu открывает панель технического администратора.
func (app *App) techMenu(ctx context.Context, bctx BotContext) error {
	var zones, extraFields int
	_ = app.queryRow(`SELECT COUNT(*) FROM zones WHERE is_active = 1`).Scan(&zones)
	_ = app.queryRow(`SELECT COUNT(*) FROM extra_field_definitions WHERE is_active = 1`).Scan(&extraFields)
	text := fmt.Sprintf("Техадмин\nАктивных зон: %d\nДоп. полей формы: %d", zones, extraFields)
	return app.reply(ctx, bctx, text, [][]Button{
		{btn("Зоны", "tech:zones", ""), btn("Доп. поля", "tech:extra_fields", "")},
		{btn("Обычные админы", "tech:admins", "")},
		{btn("Тех админы", "tech:tech_admins", "")},
		{btn("Главное меню", "menu", "")},
	})
}

// showZones показывает список корпусов и их состояние.
func (app *App) showZones(ctx context.Context, bctx BotContext) error {
	rows, err := app.query(`SELECT id, code, short_name, address, is_active, sort_order FROM zones ORDER BY sort_order ASC, short_name ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var lines []string
	var buttons [][]Button
	for rows.Next() {
		var zone ZoneRow
		if err := rows.Scan(&zone.ID, &zone.Code, &zone.ShortName, &zone.Address, &zone.IsActive, &zone.SortOrder); err != nil {
			return err
		}
		prefix := "-"
		label := "Выкл"
		if zone.IsActive == 1 {
			prefix = "+"
			label = "Вкл"
		}
		lines = append(lines, fmt.Sprintf("%s %s - %s", prefix, zone.ShortName, zone.Address))
		buttons = append(buttons, []Button{btn(label+": "+zone.ShortName, fmt.Sprintf("tech:toggle_zone:%d", zone.ID), "")})
	}
	buttons = append(buttons, []Button{btn("Добавить зону", "tech:add_zone", "")}, []Button{btn("Меню тех админа", "tech:menu", ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, strings.Join(lines, "\n"), buttons)
}

// showAdmins показывает список пользователей с выбранной ролью.
func (app *App) showAdmins(ctx context.Context, bctx BotContext, role string) error {
	title := "Обычные админы"
	if role == roleTechAdmin {
		title = "Технические админы"
	}
	rows, err := app.query(`
		SELECT id, max_user_id, display_name, role
		FROM users
		WHERE role = ?
		ORDER BY display_name ASC
	`, role)
	if err != nil {
		return err
	}

	var lines []string
	var buttons [][]Button
	for rows.Next() {
		var user UserRow
		if err := rows.Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role); err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s - %s (%d)", roleLabel(user.Role), user.DisplayName, user.MaxUserID))
		buttons = append(buttons, []Button{btn(user.DisplayName, fmt.Sprintf("tech:admin:%d", user.MaxUserID), "")})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(lines) == 0 {
		lines = []string{"Список пока пуст."}
	}
	if role == roleAdmin {
		buttons = append(buttons, []Button{btn("Добавить админа", "tech:grant_admin", "positive")})
	}
	buttons = append(buttons, []Button{btn("Меню тех админа", "tech:menu", ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, title+"\n\n"+strings.Join(lines, "\n"), buttons)
}

// showAdminDetails показывает карточку администратора и действия с ним.
func (app *App) showAdminDetails(ctx context.Context, bctx BotContext, maxUserID int64) error {
	user, err := app.userByMaxID(maxUserID)
	if err != nil {
		return err
	}
	if user == nil || (user.Role != roleAdmin && user.Role != roleTechAdmin) {
		return app.reply(ctx, bctx, "Администратор не найден.", techBackRows())
	}

	var actions int
	_ = app.queryRow(`SELECT COUNT(*) FROM audit_log WHERE actor_max_user_id = ?`, maxUserID).Scan(&actions)
	text := strings.Join([]string{
		"Администратор",
		"",
		"Имя: " + user.DisplayName,
		fmt.Sprintf("MAX user id: %d", user.MaxUserID),
		"Роль: " + roleLabel(user.Role),
		fmt.Sprintf("Действий в аудите: %d", actions),
	}, "\n")

	rows := [][]Button{{btn("Действия", fmt.Sprintf("tech:admin_audit:%d", maxUserID), "")}}
	if user.Role == roleAdmin {
		rows = append(rows, []Button{btn("Забрать роль админа", fmt.Sprintf("tech:revoke_admin:%d", maxUserID), "negative")})
	}
	backPayload := "tech:admins"
	backText := "Обычные админы"
	if user.Role == roleTechAdmin {
		backPayload = "tech:tech_admins"
		backText = "Тех админы"
	}
	rows = append(rows, []Button{btn(backText, backPayload, ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, text, rows)
}

// showAdminAudit показывает последние действия выбранного администратора.
func (app *App) showAdminAudit(ctx context.Context, bctx BotContext, maxUserID int64) error {
	if bctx.CallbackID != "" {
		if app.api != nil {
			if err := app.api.AnswerCallback(ctx, bctx.CallbackID, "Открываю список действий...", nil); err != nil {
				return err
			}
		} else {
			app.testReplies = append(app.testReplies, TestReply{Text: "Открываю список действий..."})
		}
		bctx.CallbackID = ""
	}

	user, err := app.userByMaxID(maxUserID)
	if err != nil {
		return err
	}
	if user == nil {
		return app.reply(ctx, bctx, "Пользователь не найден.", techBackRows())
	}

	auditRows, err := app.query(`
		SELECT action, COALESCE(entity_type, ''), COALESCE(entity_id, 0), created_at
		FROM audit_log
		WHERE actor_max_user_id = ?
		ORDER BY created_at DESC
		LIMIT 20
	`, maxUserID)
	if err != nil {
		return err
	}

	var lines []string
	for auditRows.Next() {
		var action, entityType, created string
		var entityID int64
		if err := auditRows.Scan(&action, &entityType, &entityID, &created); err != nil {
			_ = auditRows.Close()
			return err
		}
		entity := entityType
		if entityID > 0 {
			entity = fmt.Sprintf("%s #%d", entityType, entityID)
		}
		lines = append(lines, fmt.Sprintf("%s - %s %s", formatDateTime(created), actionLabel(action), entity))
	}
	if err := auditRows.Err(); err != nil {
		_ = auditRows.Close()
		return err
	}
	_ = auditRows.Close()
	if len(lines) == 0 {
		lines = []string{"Действий пока нет."}
	}
	return app.reply(ctx, bctx, "Действия администратора "+user.DisplayName+"\n\n"+strings.Join(lines, "\n"), [][]Button{
		{btn("Назад к админу", fmt.Sprintf("tech:admin:%d", maxUserID), ""), btn("Меню тех админа", "tech:menu", "")},
		{btn("Главное меню", "menu", "")},
	})
}

// revokeAdmin снимает роль администратора и пишет это в аудит.
func (app *App) revokeAdmin(ctx context.Context, bctx BotContext, actor UserRow, maxUserID int64) error {
	user, err := app.userByMaxID(maxUserID)
	if err != nil {
		return err
	}
	if user == nil {
		return app.reply(ctx, bctx, "Пользователь не найден.", techBackRows())
	}
	if user.Role == roleTechAdmin {
		// Техадмина нельзя снять обычным действием из бота: это защищает проект
		// от сценария, где один привилегированный пользователь гасит всех остальных.
		return app.reply(ctx, bctx, "Роль технического администратора через это меню не отзывается.", techBackRows())
	}
	if user.Role != roleAdmin {
		return app.reply(ctx, bctx, "У пользователя нет роли администратора.", techBackRows())
	}
	if _, err := app.exec(`UPDATE users SET role = ? WHERE max_user_id = ?`, roleInitiator, maxUserID); err != nil {
		return err
	}
	app.audit(actor.MaxUserID, "admin_role_revoked", "user", maxUserID, nil)
	app.sendUserNotification(ctx, maxUserID, "У вас отозвана роль администратора в сервисе «Электронное бюро пропусков».")
	return app.reply(ctx, bctx, fmt.Sprintf("Роль администратора отозвана у пользователя %d.", maxUserID), techBackRows())
}

// grantAdminFromText выдает роль администратора по введенному MAX user id.
func (app *App) grantAdminFromText(ctx context.Context, bctx BotContext, actor UserRow, text string) error {
	targetID, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || targetID <= 0 {
		return app.reply(ctx, bctx, "Введите числовой MAX user id.", techBackRows())
	}
	if targetID == actor.MaxUserID {
		return app.reply(ctx, bctx, "Нельзя выдать роль самому себе через этот сценарий.", techBackRows())
	}

	target, err := app.userByMaxID(targetID)
	if err != nil {
		return err
	}
	if target == nil {
		return app.reply(ctx, bctx, "Пользователь с таким MAX user id не найден. Попросите его сначала написать боту /start или /whoami.", techBackRows())
	}
	if target.Role == roleTechAdmin {
		return app.reply(ctx, bctx, "Этот пользователь уже технический администратор.", techBackRows())
	}
	if target.Role == roleAdmin {
		return app.reply(ctx, bctx, "Этот пользователь уже администратор.", techBackRows())
	}
	if _, err := app.exec(`UPDATE users SET role = ? WHERE max_user_id = ?`, roleAdmin, targetID); err != nil {
		return err
	}
	app.audit(actor.MaxUserID, "admin_role_granted", "user", targetID, nil)
	if err := app.sendUserNotification(ctx, targetID, "Вам выдана роль администратора в сервисе «Электронное бюро пропусков». Откройте /start, чтобы увидеть админское меню."); err != nil {
		log.Printf("notify new admin %d: %v", targetID, err)
		return app.reply(ctx, bctx, fmt.Sprintf("Роль администратора выдана пользователю %d, но уведомление отправить не удалось.", targetID), techBackRows())
	}
	return app.reply(ctx, bctx, fmt.Sprintf("Роль администратора выдана пользователю %d.", targetID), techBackRows())
}

// zoneButtons собирает кнопки активных корпусов для анкеты.
func (app *App) zoneButtons() ([][]Button, error) {
	return app.zoneButtonsWithAction("zone")
}

// zoneButtonsWithAction собирает кнопки корпусов с нужным callback-действием.
func (app *App) zoneButtonsWithAction(action string) ([][]Button, error) {
	rows, err := app.query(`SELECT id, short_name FROM zones WHERE is_active = 1 ORDER BY sort_order ASC, short_name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result [][]Button
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		result = append(result, []Button{btn(name, fmt.Sprintf("draft:%s:%d", action, id), "")})
	}
	return result, rows.Err()
}

// zoneName возвращает название корпуса по id.
func (app *App) zoneName(id int64) string {
	var name string
	if err := app.queryRow(`SELECT short_name FROM zones WHERE id = ?`, id).Scan(&name); err != nil {
		return "не указано"
	}
	return name
}

// requestZone выбирает корпус заявки из справочника или свободного текста.
func (app *App) requestZone(req RequestRow) string {
	if req.ZoneName.Valid && req.ZoneName.String != "" {
		return req.ZoneName.String
	}
	if req.CustomZoneText.Valid && req.CustomZoneText.String != "" {
		return req.CustomZoneText.String
	}
	return "Не указано"
}
