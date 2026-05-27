package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type scannerRequest struct {
	QR string `json:"qr"`
}

type scannerResponse struct {
	OK          bool   `json:"ok"`
	CanEnter    bool   `json:"can_enter"`
	RequestID   int64  `json:"request_id,omitempty"`
	Number      string `json:"number,omitempty"`
	Status      string `json:"status,omitempty"`
	FullName    string `json:"full_name,omitempty"`
	Date        string `json:"date,omitempty"`
	Time        string `json:"time,omitempty"`
	Zone        string `json:"zone,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
	Entries     int    `json:"entries"`
	Message     string `json:"message"`
	EntryResult string `json:"entry_result,omitempty"`
}

// registerScannerRoutes добавляет web-страницу сканера и его JSON API.
func (app *App) registerScannerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/scanner", app.scannerPageHandler)
	mux.HandleFunc("/scanner/api/verify", app.scannerVerifyHandler)
	mux.HandleFunc("/scanner/api/entry", app.scannerEntryHandler)
}

// scannerPageHandler отдаёт тестовую страницу для проверки QR через камеру.
func (app *App) scannerPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scannerPageHTML))
}

// scannerVerifyHandler проверяет QR и возвращает карточку пропуска.
func (app *App) scannerVerifyHandler(w http.ResponseWriter, r *http.Request) {
	if !app.scannerAuthorized(r) {
		writeScannerJSON(w, http.StatusUnauthorized, scannerResponse{Message: "Нет доступа к сканеру."})
		return
	}
	var input scannerRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeScannerJSON(w, http.StatusBadRequest, scannerResponse{Message: "Не удалось прочитать QR."})
		return
	}
	result, err := app.verifyScannerPayload(r.Context(), input.QR)
	if err != nil {
		writeScannerJSON(w, http.StatusOK, scannerResponse{Message: err.Error()})
		return
	}
	writeScannerJSON(w, http.StatusOK, result)
}

// scannerEntryHandler фиксирует проход после успешной проверки QR.
func (app *App) scannerEntryHandler(w http.ResponseWriter, r *http.Request) {
	if !app.scannerAuthorized(r) {
		writeScannerJSON(w, http.StatusUnauthorized, scannerResponse{Message: "Нет доступа к сканеру."})
		return
	}
	var input scannerRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeScannerJSON(w, http.StatusBadRequest, scannerResponse{Message: "Не удалось прочитать QR."})
		return
	}
	result, err := app.verifyScannerPayload(r.Context(), input.QR)
	if err != nil {
		writeScannerJSON(w, http.StatusOK, scannerResponse{Message: err.Error()})
		return
	}
	if !result.CanEnter {
		writeScannerJSON(w, http.StatusOK, result)
		return
	}
	actor, err := app.ensureScannerActor()
	if err != nil {
		writeScannerJSON(w, http.StatusInternalServerError, scannerResponse{Message: err.Error()})
		return
	}
	entryType, err := app.registerEntry(result.RequestID, actor)
	if err != nil {
		writeScannerJSON(w, http.StatusOK, scannerResponse{Message: err.Error()})
		return
	}
	result.EntryResult = entryLabel(entryType)
	result.Entries = app.entryCount(result.RequestID)
	result.Message = "Проход подтверждён: " + entryLabel(entryType) + "."
	writeScannerJSON(w, http.StatusOK, result)
}

// scannerAuthorized проверяет токен доступа к web-сканеру.
func (app *App) scannerAuthorized(r *http.Request) bool {
	expected := strings.TrimSpace(app.cfg.ScannerToken)
	if expected == "" {
		return false
	}
	got := strings.TrimSpace(r.URL.Query().Get("key"))
	if got == "" {
		got = strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
	}
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-Scanner-Token"))
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// verifyScannerPayload проверяет подпись QR, заявку, дату и статус.
func (app *App) verifyScannerPayload(ctx context.Context, payload string) (scannerResponse, error) {
	if err := app.expireOldRequests(ctx); err != nil {
		return scannerResponse{}, err
	}
	number, err := app.verifyPassQR(payload)
	if err != nil {
		return scannerResponse{}, err
	}
	req, err := app.requestByNumber(number)
	if err != nil {
		return scannerResponse{}, err
	}
	if req == nil {
		return scannerResponse{}, errors.New("Заявка не найдена.")
	}

	result := scannerResponse{
		OK:        true,
		RequestID: req.ID,
		Number:    req.RequestNumber,
		Status:    statusLabel(req.Status),
		FullName:  req.FullName,
		Date:      formatDate(req.VisitDate),
		Time:      req.VisitTime,
		Zone:      app.requestZone(*req),
		Purpose:   req.VisitPurpose,
		Entries:   app.entryCount(req.ID),
	}
	switch {
	case req.VisitDate != todayMoscow():
		result.Message = "Пропуск действует в другую дату."
	case req.Status == "approved" || req.Status == "passed":
		result.CanEnter = true
		result.Message = "Пропуск действителен. Можно подтвердить проход."
	default:
		result.Message = fmt.Sprintf("Проход недоступен. Текущий статус: %s.", statusLabel(req.Status))
	}
	return result, nil
}

// ensureScannerActor создаёт системного пользователя, от имени которого пишет web-сканер.
func (app *App) ensureScannerActor() (UserRow, error) {
	now := nowISO()
	var user UserRow
	err := app.queryRow(`
		INSERT INTO users (max_user_id, display_name, role, created_at)
		VALUES (0, 'Web-сканер', 'admin', ?)
		ON CONFLICT(max_user_id) DO UPDATE SET display_name = excluded.display_name, role = excluded.role
		RETURNING id, max_user_id, display_name, role
	`, now).Scan(&user.ID, &user.MaxUserID, &user.DisplayName, &user.Role)
	return user, err
}

// writeScannerJSON отправляет ответ API сканера в едином формате.
func writeScannerJSON(w http.ResponseWriter, status int, payload scannerResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

const scannerPageHTML = `<!doctype html>
<html lang="ru">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Сканер пропусков</title>
  <style>
    :root { color-scheme: light; font-family: Inter, system-ui, -apple-system, Segoe UI, sans-serif; }
    body { margin: 0; background: #f4f6f8; color: #17202a; }
    main { max-width: 920px; margin: 0 auto; padding: 24px; }
    h1 { margin: 0 0 16px; font-size: 28px; }
    section { background: #fff; border: 1px solid #dce3ea; border-radius: 8px; padding: 16px; margin-bottom: 16px; }
    label { display: block; font-weight: 650; margin-bottom: 6px; }
    input, textarea { width: 100%; box-sizing: border-box; border: 1px solid #c6d0da; border-radius: 6px; padding: 10px 12px; font: inherit; }
    textarea { min-height: 86px; resize: vertical; }
    video { width: 100%; max-height: 420px; background: #111; border-radius: 8px; margin-top: 12px; }
    button { border: 0; border-radius: 6px; padding: 10px 14px; font: inherit; font-weight: 650; cursor: pointer; background: #2457c5; color: white; }
    button.secondary { background: #e8edf4; color: #17202a; }
    button.positive { background: #197a4d; }
    button:disabled { opacity: .55; cursor: not-allowed; }
    .row { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 12px; }
    .status { padding: 12px; border-radius: 8px; background: #eef3f8; white-space: pre-wrap; }
    .ok { background: #e8f6ef; }
    .bad { background: #fdeeee; }
    dl { display: grid; grid-template-columns: 160px 1fr; gap: 8px 12px; }
    dt { font-weight: 650; color: #526071; }
    dd { margin: 0; }
    @media (max-width: 640px) { main { padding: 14px; } dl { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
<main>
  <h1>Сканер пропусков</h1>
  <section>
    <label for="token">Ключ сканера</label>
    <input id="token" type="password" autocomplete="off" placeholder="SCANNER_ACCESS_TOKEN">
    <div class="row">
      <button id="saveToken" class="secondary">Сохранить ключ</button>
      <button id="startCamera">Включить камеру</button>
      <button id="stopCamera" class="secondary">Остановить</button>
    </div>
    <video id="video" playsinline muted></video>
  </section>
  <section>
    <label for="manual">QR вручную</label>
    <textarea id="manual" placeholder="Вставьте текст из QR, если камера недоступна"></textarea>
    <div class="row">
      <button id="checkManual">Проверить</button>
      <button id="confirmEntry" class="positive" disabled>Подтвердить проход</button>
    </div>
  </section>
  <section>
    <div id="status" class="status">Ожидание QR-кода.</div>
    <div id="card"></div>
  </section>
</main>
<script>
const token = document.querySelector('#token');
const manual = document.querySelector('#manual');
const statusBox = document.querySelector('#status');
const card = document.querySelector('#card');
const confirmButton = document.querySelector('#confirmEntry');
const video = document.querySelector('#video');
let lastQR = '';
let stream = null;
let scanning = false;

token.value = new URLSearchParams(location.search).get('key') || localStorage.getItem('scannerToken') || '';
document.querySelector('#saveToken').onclick = () => {
  localStorage.setItem('scannerToken', token.value.trim());
  showStatus('Ключ сохранён.', true);
};
document.querySelector('#checkManual').onclick = () => verify(manual.value.trim());
confirmButton.onclick = () => confirmEntry();
document.querySelector('#startCamera').onclick = () => startCamera();
document.querySelector('#stopCamera').onclick = () => stopCamera();

function showStatus(text, ok) {
  statusBox.textContent = text;
  statusBox.className = 'status ' + (ok ? 'ok' : 'bad');
}

function render(data) {
  confirmButton.disabled = !data.can_enter;
  card.innerHTML = '';
  if (!data.ok) {
    showStatus(data.message || 'QR не прошёл проверку.', false);
    return;
  }
  showStatus(data.message, data.can_enter);
  card.innerHTML = '<dl>' +
    '<dt>Заявка</dt><dd>' + escapeHtml(data.number) + '</dd>' +
    '<dt>Статус</dt><dd>' + escapeHtml(data.status) + '</dd>' +
    '<dt>ФИО</dt><dd>' + escapeHtml(data.full_name) + '</dd>' +
    '<dt>Дата и время</dt><dd>' + escapeHtml(data.date + ' ' + data.time) + '</dd>' +
    '<dt>Корпус</dt><dd>' + escapeHtml(data.zone) + '</dd>' +
    '<dt>Цель</dt><dd>' + escapeHtml(data.purpose) + '</dd>' +
    '<dt>Проходы</dt><dd>' + String(data.entries || 0) + '</dd>' +
    '</dl>';
}

async function verify(qr) {
  if (!qr) { showStatus('QR пустой.', false); return; }
  lastQR = qr;
  const data = await callApi('/scanner/api/verify', qr);
  render(data);
}

async function confirmEntry() {
  if (!lastQR) { return; }
  const data = await callApi('/scanner/api/entry', lastQR);
  render(data);
}

async function callApi(path, qr) {
  const key = token.value.trim();
  const res = await fetch(path + '?key=' + encodeURIComponent(key), {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({qr})
  });
  return await res.json();
}

async function startCamera() {
  if (!('BarcodeDetector' in window)) {
    showStatus('Браузер не поддерживает BarcodeDetector. Используйте ручной ввод.', false);
    return;
  }
  stream = await navigator.mediaDevices.getUserMedia({video: {facingMode: 'environment'}});
  video.srcObject = stream;
  await video.play();
  scanning = true;
  const detector = new BarcodeDetector({formats: ['qr_code']});
  while (scanning) {
    const codes = await detector.detect(video).catch(() => []);
    if (codes.length > 0) {
      await verify(codes[0].rawValue);
      await new Promise(resolve => setTimeout(resolve, 1600));
    }
    await new Promise(resolve => requestAnimationFrame(resolve));
  }
}

function stopCamera() {
  scanning = false;
  if (stream) {
    stream.getTracks().forEach(track => track.stop());
    stream = null;
  }
}

function escapeHtml(value) {
  return String(value || '').replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
}
</script>
</body>
</html>`
