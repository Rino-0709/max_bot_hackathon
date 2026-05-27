package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

const qrPayloadPrefix = "SCP1"

// qrSecret возвращает секрет для подписи QR без вывода его в интерфейс.
func (app *App) qrSecret() string {
	if strings.TrimSpace(app.cfg.QRSecret) != "" {
		return strings.TrimSpace(app.cfg.QRSecret)
	}
	return strings.TrimSpace(app.cfg.Token)
}

// qrSignature подписывает номер заявки, чтобы QR нельзя было подобрать вручную.
func (app *App) qrSignature(requestNumber string) (string, error) {
	secret := app.qrSecret()
	if secret == "" {
		return "", errors.New("QR_SECRET is empty")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(requestNumber))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32], nil
}

// passQRPayload собирает короткое содержимое QR без персональных данных.
func (app *App) passQRPayload(requestNumber string) (string, error) {
	signature, err := app.qrSignature(requestNumber)
	if err != nil {
		return "", err
	}
	return qrPayloadPrefix + "|" + requestNumber + "|" + signature, nil
}

// verifyPassQR проверяет подпись QR и возвращает номер заявки.
func (app *App) verifyPassQR(payload string) (string, error) {
	parts := strings.Split(strings.TrimSpace(payload), "|")
	if len(parts) != 3 || parts[0] != qrPayloadPrefix {
		return "", errors.New("QR-код не похож на пропуск.")
	}
	expected, err := app.qrSignature(parts[1])
	if err != nil {
		return "", err
	}
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", errors.New("Подпись QR-кода не прошла проверку.")
	}
	return parts[1], nil
}

// passQRCodePNG генерирует PNG с QR-кодом заявки.
func (app *App) passQRCodePNG(requestNumber string) ([]byte, error) {
	payload, err := app.passQRPayload(requestNumber)
	if err != nil {
		return nil, err
	}
	return qrcode.Encode(payload, qrcode.Medium, 320)
}
