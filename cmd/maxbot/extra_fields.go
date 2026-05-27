package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
)

// activeExtraFields возвращает включенные дополнительные поля формы.
func (app *App) activeExtraFields() ([]ExtraFieldRow, error) {
	rows, err := app.query(`
		SELECT id, label, is_active, sort_order
		FROM extra_field_definitions
		WHERE is_active = 1
		ORDER BY sort_order ASC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExtraFields(rows)
}

// allExtraFields возвращает весь справочник допполей, включая выключенные.
func (app *App) allExtraFields() ([]ExtraFieldRow, error) {
	rows, err := app.query(`
		SELECT id, label, is_active, sort_order
		FROM extra_field_definitions
		ORDER BY sort_order ASC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExtraFields(rows)
}

// scanExtraFields читает допполя из результата SQL-запроса.
func scanExtraFields(rows *sql.Rows) ([]ExtraFieldRow, error) {
	var result []ExtraFieldRow
	for rows.Next() {
		var field ExtraFieldRow
		if err := rows.Scan(&field.ID, &field.Label, &field.IsActive, &field.SortOrder); err != nil {
			return nil, err
		}
		result = append(result, field)
	}
	return result, rows.Err()
}

// showExtraFields показывает техадмину список и состояние допполей.
func (app *App) showExtraFields(ctx context.Context, bctx BotContext) error {
	fields, err := app.allExtraFields()
	if err != nil {
		return err
	}
	var lines []string
	var buttons [][]Button
	for _, field := range fields {
		mark := "-"
		action := "Выкл"
		if field.IsActive == 1 {
			mark = "+"
			action = "Вкл"
		}
		lines = append(lines, fmt.Sprintf("%s %s", mark, field.Label))
		buttons = append(buttons, []Button{btn(action+": "+field.Label, fmt.Sprintf("tech:toggle_extra_field:%d", field.ID), "")})
	}
	if len(lines) == 0 {
		lines = []string{"Дополнительных полей пока нет."}
	}
	buttons = append(buttons,
		[]Button{btn("Добавить поля", "tech:add_extra_fields", "positive")},
		[]Button{btn("Меню тех админа", "tech:menu", ""), btn("Главное меню", "menu", "")},
	)
	return app.reply(ctx, bctx, "Дополнительные поля формы\n\n"+strings.Join(lines, "\n"), buttons)
}

// addExtraFieldsFromText добавляет допполя из строк, которые ввел техадмин.
func (app *App) addExtraFieldsFromText(ctx context.Context, bctx BotContext, actor UserRow, text string) error {
	labels := parseExtraFieldLabels(text)
	if len(labels) == 0 {
		return app.reply(ctx, bctx, "Не нашёл названия полей. Напишите каждое поле с новой строки или через запятую.", techBackRows())
	}
	var sortOrder int
	_ = app.queryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM extra_field_definitions`).Scan(&sortOrder)

	added := 0
	for _, label := range labels {
		sortOrder++
		res, err := app.exec(`
			INSERT INTO extra_field_definitions (label, is_active, sort_order, created_at)
			VALUES (?, 1, ?, ?)
			ON CONFLICT(label) DO UPDATE SET is_active = 1
		`, label, sortOrder, nowISO())
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			added++
		}
	}
	app.audit(actor.MaxUserID, "extra_fields_added", "extra_field", 0, map[string]string{"count": fmt.Sprint(added)})
	return app.showExtraFields(ctx, bctx)
}

// parseExtraFieldLabels режет сообщение техадмина на названия полей.
func parseExtraFieldLabels(text string) []string {
	parts := regexp.MustCompile(`[\n,;]+`).Split(text, -1)
	seen := map[string]bool{}
	var result []string
	for _, part := range parts {
		label := normalizeSpaces(part)
		label = strings.Trim(label, ".:-")
		if label == "" || len([]rune(label)) > 80 {
			continue
		}
		key := strings.ToLower(label)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, label)
	}
	return result
}

// toggleExtraField включает или выключает допполе, не удаляя старые ответы.
func (app *App) toggleExtraField(ctx context.Context, bctx BotContext, actor UserRow, fieldID int64) error {
	var active int
	if err := app.queryRow(`SELECT is_active FROM extra_field_definitions WHERE id = ?`, fieldID).Scan(&active); err != nil {
		return app.reply(ctx, bctx, "Дополнительное поле не найдено.", techBackRows())
	}
	next := 1
	if active == 1 {
		next = 0
	}
	if _, err := app.exec(`UPDATE extra_field_definitions SET is_active = ? WHERE id = ?`, next, fieldID); err != nil {
		return err
	}
	app.audit(actor.MaxUserID, "extra_field_toggled", "extra_field", fieldID, map[string]string{"active": fmt.Sprint(next)})
	return app.showExtraFields(ctx, bctx)
}

// askNextExtraFieldOrSummary ведет гостя по допполям и потом показывает сводку.
func (app *App) askNextExtraFieldOrSummary(ctx context.Context, bctx BotContext, user UserRow) error {
	field, ok, err := app.nextMissingExtraField(user.ID)
	if err != nil {
		return err
	}
	if !ok {
		return app.showDraftSummary(ctx, bctx, user)
	}
	app.setSession(user.MaxUserID, "draft_extra_field", map[string]string{
		"field_id": fmt.Sprint(field.ID),
		"label":    field.Label,
	})
	return app.reply(ctx, bctx, "Заполните дополнительное поле:\n\n"+field.Label, draftBackRows("draft:back_purpose"))
}

// nextMissingExtraField ищет следующее незаполненное допполе в черновике.
func (app *App) nextMissingExtraField(userID int64) (ExtraFieldRow, bool, error) {
	fields, err := app.activeExtraFields()
	if err != nil || len(fields) == 0 {
		return ExtraFieldRow{}, false, err
	}
	draft, err := app.getDraft(userID)
	if err != nil || draft == nil {
		return ExtraFieldRow{}, false, err
	}
	values := parseExtraFieldValues(draft.ExtraFieldsJSON.String)
	for _, field := range fields {
		if strings.TrimSpace(extraFieldValue(values, field.ID, field.Label)) == "" {
			return field, true, nil
		}
	}
	return ExtraFieldRow{}, false, nil
}

// setDraftExtraField сохраняет ответ гостя на дополнительное поле в черновике.
func (app *App) setDraftExtraField(userID int64, fieldID int64, label, value string) error {
	draft, err := app.getDraft(userID)
	if err != nil {
		return err
	}
	var current string
	if draft != nil && draft.ExtraFieldsJSON.Valid {
		current = draft.ExtraFieldsJSON.String
	}
	values := parseExtraFieldValues(current)
	updated := false
	for i := range values {
		if values[i].ID == fieldID {
			values[i].Label = label
			values[i].Value = value
			updated = true
			break
		}
	}
	if !updated {
		values = append(values, ExtraFieldValue{ID: fieldID, Label: label, Value: value})
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	return app.updateDraft(userID, map[string]interface{}{"extra_fields_json": string(raw)})
}

// parseExtraFieldValues разбирает JSON с ответами на допполя.
func parseExtraFieldValues(raw string) []ExtraFieldValue {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var values []ExtraFieldValue
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		log.Printf("parse extra fields json: %v", err)
		return nil
	}
	return values
}

// extraFieldValue достает сохраненный ответ по id или названию поля.
func extraFieldValue(values []ExtraFieldValue, id int64, label string) string {
	for _, item := range values {
		if item.ID == id || strings.EqualFold(item.Label, label) {
			return item.Value
		}
	}
	return ""
}

// formatExtraFieldsForText превращает ответы на допполя в строки карточки.
func formatExtraFieldsForText(raw string) string {
	values := parseExtraFieldValues(raw)
	if len(values) == 0 {
		return ""
	}
	var lines []string
	for _, item := range values {
		value := strings.TrimSpace(item.Value)
		if value == "" {
			continue
		}
		lines = append(lines, item.Label+": "+value)
	}
	return strings.Join(lines, "\n")
}
