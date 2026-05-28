package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	scannerAdminTokenPrefix = "adm"
	testScannerMaxUserID    = -1
)

// scannerTokenFromRequest достает ключ сканера из query-параметра или HTTP-заголовков.
func scannerTokenFromRequest(r *http.Request) string {
	token := strings.TrimSpace(r.URL.Query().Get("key"))
	if token == "" {
		token = strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Scanner-Token"))
	}
	return strings.TrimSpace(token)
}

// scannerSigningSecret выбирает секрет, которым подписываются личные ключи админов.
func (app *App) scannerSigningSecret() string {
	if secret := strings.TrimSpace(app.cfg.QRSecret); secret != "" {
		return secret
	}
	return strings.TrimSpace(app.cfg.ScannerToken)
}

// scannerAdminTokenForID собирает личный ключ сканера для конкретного MAX user id.
func (app *App) scannerAdminTokenForID(maxUserID int64) (string, error) {
	secret := app.scannerSigningSecret()
	if secret == "" {
		return "", errors.New("не задан секрет для ключей сканера")
	}
	payload := fmt.Sprintf("%s.%d", scannerAdminTokenPrefix, maxUserID)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
	return payload + "." + signature, nil
}

// scannerAdminToken возвращает личный ключ текущего администратора.
func (app *App) scannerAdminToken(user UserRow) (string, error) {
	if !app.isAdmin(user) {
		return "", errors.New("ключ сканера доступен только администратору")
	}
	return app.scannerAdminTokenForID(user.MaxUserID)
}

// scannerPrincipal определяет, от чьего имени работает web-сканер.
func (app *App) scannerPrincipal(r *http.Request) (UserRow, bool) {
	token := scannerTokenFromRequest(r)
	if token == "" {
		return UserRow{}, false
	}

	if app.constantTokenEqual(token, app.cfg.ScannerTestToken) {
		user, err := app.ensureTestScannerActor()
		if err != nil {
			return UserRow{}, false
		}
		return user, true
	}

	if app.constantTokenEqual(token, app.cfg.ScannerToken) {
		user, err := app.ensureScannerActor()
		if err != nil {
			return UserRow{}, false
		}
		return user, true
	}

	user, err := app.adminFromScannerToken(token)
	if err != nil || user == nil {
		return UserRow{}, false
	}
	return *user, true
}

// constantTokenEqual сравнивает ключи без раннего выхода по первому отличию.
func (app *App) constantTokenEqual(got, expected string) bool {
	expected = strings.TrimSpace(expected)
	if got == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// adminFromScannerToken проверяет подпись личного ключа и роль пользователя.
func (app *App) adminFromScannerToken(token string) (*UserRow, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != scannerAdminTokenPrefix {
		return nil, errors.New("неизвестный формат ключа сканера")
	}
	maxUserID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || maxUserID <= 0 {
		return nil, errors.New("ключ сканера содержит некорректный MAX user id")
	}
	expected, err := app.scannerAdminTokenForID(maxUserID)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		return nil, errors.New("подпись ключа сканера не прошла проверку")
	}
	user, err := app.userByMaxID(maxUserID)
	if err != nil {
		return nil, err
	}
	if user == nil || !app.isAdmin(*user) {
		return nil, errors.New("ключ принадлежит пользователю без роли администратора")
	}
	return user, nil
}

// scannerURLWithKey собирает ссылку на web-сканер сразу с нужным ключом.
func (app *App) scannerURLWithKey(token string) string {
	base := strings.TrimSpace(app.cfg.ScannerPublicURL)
	if base == "" {
		base = "/scanner"
	}
	if strings.Contains(base, "?") {
		return base + "&key=" + url.QueryEscape(token)
	}
	return base + "?key=" + url.QueryEscape(token)
}

// showScannerKey отправляет администратору его личный ключ и готовую ссылку на сканер.
func (app *App) showScannerKey(ctx context.Context, bctx BotContext, user UserRow) error {
	token, err := app.scannerAdminToken(user)
	if err != nil {
		return app.reply(ctx, bctx, err.Error(), adminBackRows())
	}
	text := strings.Join([]string{
		"Ключ сканера",
		"",
		"Это ваш личный ключ для web-сканера. Все подтвержденные через него проходы будут записываться на вас.",
		"",
		"Ссылка:",
		app.scannerURLWithKey(token),
		"",
		"Ключ:",
		token,
		"",
		"Не пересылайте этот ключ другим администраторам.",
	}, "\n")
	return app.reply(ctx, bctx, text, adminBackRows())
}

// ensureTestScannerActor создает отдельного системного пользователя для общего демо-ключа.
func (app *App) ensureTestScannerActor() (UserRow, error) {
	now := nowISO()
	var user UserRow
	err := app.queryRow(`
		INSERT INTO users (max_user_id, display_name, role, created_at)
		VALUES (?, 'Демо-ключ сканера', 'admin', ?)
		ON CONFLICT(max_user_id) DO UPDATE SET display_name = excluded.display_name, role = excluded.role
		RETURNING id, max_user_id, display_name, role
	`, testScannerMaxUserID, now).Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role)
	return user, err
}
