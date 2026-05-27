package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
)

// adminMenu открывает стартовую панель администратора.
func (app *App) adminMenu(ctx context.Context, bctx BotContext) error {
	if err := app.expireOldRequests(ctx); err != nil {
		log.Printf("expire old requests: %v", err)
	}
	var pending, approvedToday int
	_ = app.queryRow(`SELECT COUNT(*) FROM pass_requests WHERE status = 'pending_review' AND visit_date >= ?`, todayMoscow()).Scan(&pending)
	_ = app.queryRow(`SELECT COUNT(*) FROM pass_requests WHERE visit_date = ? AND status IN ('approved', 'passed')`, todayMoscow()).Scan(&approvedToday)
	text := fmt.Sprintf("Админ\nНа рассмотрении: %d\nАктивных сегодня: %d", pending, approvedToday)
	return app.reply(ctx, bctx, text, [][]Button{
		{btn("Очередь", "admin:queue:0", ""), btn("Активные сегодня", "admin:approved_today:0", "")},
		{btn("Поиск по номеру", "admin:search", ""), btn("Экспорт Excel", "admin:export_active_today", "")},
		{btn("Главное меню", "menu", "")},
	})
}

// adminQueue выводит заявки администратора с пагинацией и правильной сортировкой.
func (app *App) adminQueue(ctx context.Context, bctx BotContext, mode string, page int) error {
	if err := app.expireOldRequests(ctx); err != nil {
		log.Printf("expire old requests: %v", err)
	}
	where := "pr.status = 'pending_review' AND pr.visit_date >= ?"
	args := []interface{}{todayMoscow()}
	title := "Очередь заявок"
	if mode == "approved_today" {
		where = "pr.visit_date = ? AND pr.status IN ('approved', 'passed')"
		args = append(args, todayMoscow())
		title = "Активные сегодня"
	}
	limit := 5
	offset := page * limit
	queryArgs := append(args, limit, offset)
	rows, err := app.query(`
		SELECT pr.id, pr.request_number, pr.user_id, pr.full_name, pr.visit_date, pr.visit_time,
			pr.zone_id, pr.custom_zone_text, pr.visit_purpose, pr.extra_fields_json, pr.status, pr.public_comment,
			u.display_name, u.max_user_id, z.short_name, z.address, pr.created_at, pr.updated_at
		FROM pass_requests pr
		JOIN users u ON u.id = pr.user_id
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE `+where+`
		ORDER BY pr.visit_date ASC, pr.visit_time ASC, pr.created_at ASC
		LIMIT ? OFFSET ?
	`, queryArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()

	buttonRows := [][]Button{}
	var lines []string
	for rows.Next() {
		req, err := app.scanRequest(rows)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s - %s, %s %s, %s", req.RequestNumber, req.FullName, formatDate(req.VisitDate), req.VisitTime, app.requestZone(*req)))
		buttonRows = append(buttonRows, []Button{btn(req.RequestNumber, fmt.Sprintf("request:open:%d", req.ID), "")})
	}
	if len(lines) == 0 {
		lines = []string{"Заявок нет."}
	}
	nav := []Button{}
	if page > 0 {
		nav = append(nav, btn("Назад", fmt.Sprintf("admin:%s:%d", modeAction(mode), page-1), ""))
	}
	nav = append(nav, btn("Дальше", fmt.Sprintf("admin:%s:%d", modeAction(mode), page+1), ""))
	buttonRows = append(buttonRows, nav, []Button{btn("Меню админа", "admin:menu", ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, title+"\n\n"+strings.Join(lines, "\n"), buttonRows)
}

// modeAction выбирает callback для текущего режима админской очереди.
func modeAction(mode string) string {
	if mode == "approved_today" {
		return "approved_today"
	}
	return "queue"
}

// exportActiveToday отправляет администратору Excel с заявками на сегодня.
func (app *App) exportActiveToday(ctx context.Context, bctx BotContext) error {
	content, activeCount, closedCount, err := app.buildTodayRequestsWorkbook()
	if err != nil {
		return err
	}
	fileName := fmt.Sprintf("pass_requests_%s.xlsx", todayMoscow())
	if err := app.saveExportFile(fileName, content); err != nil {
		log.Printf("save export file: %v", err)
	}

	if app.api != nil {
		if bctx.CallbackID != "" {
			if err := app.api.AnswerCallback(ctx, bctx.CallbackID, fmt.Sprintf("Готовлю Excel-файл. Активных: %d, закрытых: %d.", activeCount, closedCount), adminBackRows()); err != nil {
				return err
			}
			bctx.CallbackID = ""
		}
		if err := app.api.SendFileToUser(ctx, bctx.User.UserID, fmt.Sprintf("Excel-экспорт заявок на сегодня. Активных: %d, закрытых: %d.", activeCount, closedCount), fileName, content); err != nil {
			log.Printf("send xlsx file: %v", err)
			return app.reply(ctx, bctx, "Не удалось отправить Excel-файл. Файл сохранён локально: "+exportFilePath(app.cfg.DataDir, fileName), adminBackRows())
		}
		return app.reply(ctx, bctx, "Excel-файл отправлен.", adminBackRows())
	}

	return app.reply(ctx, bctx, fmt.Sprintf("Excel-экспорт сформирован. Активных: %d, закрытых: %d.\nФайл сохранён локально: %s", activeCount, closedCount, exportFilePath(app.cfg.DataDir, fileName)), adminBackRows())
}

// buildTodayRequestsWorkbook собирает книгу Excel с активными и закрытыми заявками.
func (app *App) buildTodayRequestsWorkbook() ([]byte, int, int, error) {
	// Один файл с двумя листами удобнее для поста контроля: активные заявки
	// не смешиваются с закрытыми, но администратор всё равно получает полный
	// дневной контекст без переходов по меню бота.
	active, err := app.exportRequestsByStatuses(todayMoscow(), []string{"pending_review", "clarification_requested", "approved", "passed"})
	if err != nil {
		return nil, 0, 0, err
	}
	closed, err := app.exportRequestsByStatuses(todayMoscow(), []string{"rejected", "cancelled_by_initiator", "cancelled_by_administrator", "expired", "no_show", "closed", "data_erasure_requested"})
	if err != nil {
		return nil, 0, 0, err
	}

	workbook := excelize.NewFile()
	defer func() { _ = workbook.Close() }()

	activeSheet := "Активные заявки"
	closedSheet := "Закрытые заявки"
	if err := workbook.SetSheetName("Sheet1", activeSheet); err != nil {
		return nil, 0, 0, err
	}
	if _, err := workbook.NewSheet(closedSheet); err != nil {
		return nil, 0, 0, err
	}
	if err := writeExportSheet(workbook, activeSheet, active); err != nil {
		return nil, 0, 0, err
	}
	if err := writeExportSheet(workbook, closedSheet, closed); err != nil {
		return nil, 0, 0, err
	}
	workbook.SetActiveSheet(0)

	var buf bytes.Buffer
	if err := workbook.Write(&buf); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), len(active), len(closed), nil
}

// exportRequestsByStatuses достает заявки за дату по выбранным статусам.
func (app *App) exportRequestsByStatuses(date string, statuses []string) ([]ExportRequestRow, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(statuses)), ",")
	args := []interface{}{date}
	for _, status := range statuses {
		args = append(args, status)
	}
	rows, err := app.query(`
		SELECT pr.request_number, pr.full_name, pr.visit_date, pr.visit_time,
			COALESCE(z.short_name, pr.custom_zone_text, 'Не указано'),
			pr.visit_purpose, COALESCE(pr.extra_fields_json, ''), pr.status, COALESCE(pr.public_comment, ''), pr.updated_at
		FROM pass_requests pr
		LEFT JOIN zones z ON z.id = pr.zone_id
		WHERE pr.visit_date = ? AND pr.status IN (`+placeholders+`)
		ORDER BY pr.visit_time ASC, pr.created_at ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []ExportRequestRow
	for rows.Next() {
		var row ExportRequestRow
		if err := rows.Scan(&row.Number, &row.FullName, &row.Date, &row.Time, &row.Zone, &row.Purpose, &row.Extra, &row.Status, &row.Comment, &row.UpdatedAt); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// writeExportSheet оформляет один лист Excel-выгрузки.
func writeExportSheet(workbook *excelize.File, sheet string, rows []ExportRequestRow) error {
	headers := []string{"Номер", "ФИО", "Дата", "Время", "Зона", "Цель", "Доп. поля", "Статус", "Комментарий", "Обновлено"}
	if err := workbook.SetSheetRow(sheet, "A1", &headers); err != nil {
		return err
	}
	for index, item := range rows {
		values := []interface{}{
			item.Number,
			item.FullName,
			formatDate(item.Date),
			item.Time,
			item.Zone,
			item.Purpose,
			formatExtraFieldsForText(item.Extra),
			statusLabel(item.Status),
			item.Comment,
			formatDateTime(item.UpdatedAt),
		}
		cell := fmt.Sprintf("A%d", index+2)
		if err := workbook.SetSheetRow(sheet, cell, &values); err != nil {
			return err
		}
	}
	headerStyle, err := workbook.NewStyle(&excelize.Style{Font: &excelize.Font{Bold: true}})
	if err != nil {
		return err
	}
	if err := workbook.SetCellStyle(sheet, "A1", "J1", headerStyle); err != nil {
		return err
	}
	for _, col := range []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"} {
		if err := workbook.SetColWidth(sheet, col, col, 18); err != nil {
			return err
		}
	}
	return nil
}

// saveExportFile оставляет файл на диске, если MAX не смог принять вложение.
func (app *App) saveExportFile(fileName string, content []byte) error {
	path := exportFilePath(app.cfg.DataDir, fileName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0600)
}

// exportFilePath собирает путь к файлу выгрузки внутри data-dir.
func exportFilePath(dataDir, fileName string) string {
	baseDir := "./data"
	if strings.TrimSpace(dataDir) != "" {
		baseDir = dataDir
	}
	return filepath.Join(baseDir, "exports", filepath.Base(fileName))
}
