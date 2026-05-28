package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const demoRequestMaxUserID = -2

// showScannerTest отправляет общий демо-ключ и QR-код для проверки web-сканера.
func (app *App) showScannerTest(ctx context.Context, bctx BotContext, user UserRow) error {
	token := strings.TrimSpace(app.cfg.ScannerTestToken)
	if token == "" {
		return app.reply(ctx, bctx, "Демо-ключ сканера не настроен. Добавьте SCANNER_TEST_ACCESS_TOKEN в .env.", app.mainMenu(user))
	}
	req, err := app.ensureScannerDemoRequest()
	if err != nil {
		return err
	}
	png, err := app.passQRCodePNG(req.RequestNumber)
	if err != nil {
		return err
	}
	text := strings.Join([]string{
		"Тест сканера",
		"",
		"Эта команда нужна только для демонстрации. Ниже общий тестовый QR-код и ссылка на web-сканер.",
		"",
		"Ссылка:",
		app.scannerURLWithKey(token),
		"",
		"Ключ:",
		token,
		"",
		"Заявка: " + req.RequestNumber,
	}, "\n")
	if app.api == nil {
		app.testReplies = append(app.testReplies, TestReply{Text: text})
		return nil
	}
	return app.api.SendFileToUser(ctx, bctx.User.UserID, text, "test_pass_"+req.RequestNumber+".png", png)
}

// ensureScannerDemoRequest поддерживает одну одобренную демо-заявку на текущий день.
func (app *App) ensureScannerDemoRequest() (*RequestRow, error) {
	owner, err := app.ensureScannerDemoOwner()
	if err != nil {
		return nil, err
	}
	zoneID, err := app.firstActiveZoneID()
	if err != nil {
		return nil, err
	}
	date := todayMoscow()
	number := "TEST-" + strings.ReplaceAll(date, "-", "") + "-SCAN"
	now := nowISO()
	var requestID int64
	err = app.queryRow(`
		INSERT INTO pass_requests (
			request_number, user_id, full_name, visit_date, visit_time, zone_id,
			visit_purpose, status, public_comment, created_at, updated_at
		)
		VALUES (?, ?, 'Тестовый посетитель', ?, '13:00', ?, 'Проверка QR-сканера на презентации', 'approved', 'Демо-заявка для проверки сканера', ?, ?)
		ON CONFLICT(request_number) DO UPDATE SET
			visit_date = excluded.visit_date,
			visit_time = excluded.visit_time,
			zone_id = excluded.zone_id,
			status = CASE
				WHEN pass_requests.visit_date <> excluded.visit_date THEN 'approved'
				WHEN pass_requests.status IN ('expired', 'closed', 'rejected', 'cancelled_by_initiator', 'cancelled_by_administrator', 'no_show') THEN 'approved'
				ELSE pass_requests.status
			END,
			updated_at = excluded.updated_at
		RETURNING id
	`, number, owner.ID, date, zoneID, now, now).Scan(&requestID)
	if err != nil {
		return nil, err
	}
	req, err := app.requestByID(requestID)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("демо-заявка не найдена после создания")
	}
	return req, nil
}

// ensureScannerDemoOwner создает системного владельца демо-заявки.
func (app *App) ensureScannerDemoOwner() (UserRow, error) {
	now := nowISO()
	var user UserRow
	err := app.queryRow(`
		INSERT INTO users (max_user_id, display_name, role, created_at)
		VALUES (?, 'Демо-посетитель', 'initiator', ?)
		ON CONFLICT(max_user_id) DO UPDATE SET display_name = excluded.display_name
		RETURNING id, max_user_id, display_name, role
	`, demoRequestMaxUserID, now).Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role)
	return user, err
}

// firstActiveZoneID выбирает первую активную зону для демонстрационной заявки.
func (app *App) firstActiveZoneID() (int64, error) {
	var zoneID int64
	err := app.queryRow(`SELECT id FROM zones WHERE is_active = 1 ORDER BY sort_order ASC, short_name ASC LIMIT 1`).Scan(&zoneID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("нет активных зон для демо-заявки")
	}
	return zoneID, err
}
