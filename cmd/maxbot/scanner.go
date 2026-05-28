package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// scannerJSQR содержит локальный QR-движок для браузеров без BarcodeDetector.
//
//go:embed static/jsqr.js
var scannerJSQR []byte

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
	mux.HandleFunc("/scanner/assets/jsqr.js", app.scannerJSQRHandler)
	mux.HandleFunc("/scanner/api/verify", app.scannerVerifyHandler)
	mux.HandleFunc("/scanner/api/entry", app.scannerEntryHandler)
}

// scannerPageHandler отдаёт тестовую страницу для проверки QR через камеру.
func (app *App) scannerPageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scannerPageHTML))
}

// scannerJSQRHandler отдаёт локальный JS-декодер QR для мобильных браузеров.
func (app *App) scannerJSQRHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	_, _ = w.Write(scannerJSQR)
}

// scannerVerifyHandler проверяет QR и возвращает карточку пропуска.
func (app *App) scannerVerifyHandler(w http.ResponseWriter, r *http.Request) {
	if !app.allowScannerRequest(w, r, 90) {
		return
	}
	if _, ok := app.scannerPrincipal(r); !ok {
		writeScannerJSON(w, http.StatusUnauthorized, scannerResponse{Message: "Нет доступа к сканеру."})
		return
	}
	input, ok := readScannerRequest(w, r)
	if !ok {
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
	if !app.allowScannerRequest(w, r, 30) {
		return
	}
	actor, ok := app.scannerPrincipal(r)
	if !ok {
		writeScannerJSON(w, http.StatusUnauthorized, scannerResponse{Message: "Нет доступа к сканеру."})
		return
	}
	input, ok := readScannerRequest(w, r)
	if !ok {
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

// allowScannerRequest ограничивает частоту запросов к публичному API сканера.
func (app *App) allowScannerRequest(w http.ResponseWriter, r *http.Request, perMinute int) bool {
	if r.Method != http.MethodPost {
		writeScannerJSON(w, http.StatusMethodNotAllowed, scannerResponse{Message: "Метод не поддерживается."})
		return false
	}
	if app.scannerRate == nil {
		app.scannerRate = &RateLimiter{}
	}
	if !app.scannerRate.allow(scannerRateKey(r), perMinute, time.Minute) {
		writeScannerJSON(w, http.StatusTooManyRequests, scannerResponse{Message: "Слишком много запросов. Подождите немного и попробуйте снова."})
		return false
	}
	return true
}

// readScannerRequest читает QR из JSON и отсекает слишком большие запросы.
func readScannerRequest(w http.ResponseWriter, r *http.Request) (scannerRequest, bool) {
	var input scannerRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeScannerJSON(w, http.StatusBadRequest, scannerResponse{Message: "Не удалось прочитать QR."})
		return input, false
	}
	if len([]rune(input.QR)) > 512 {
		writeScannerJSON(w, http.StatusBadRequest, scannerResponse{Message: "QR слишком длинный."})
		return input, false
	}
	return input, true
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
    .camera-wrap { position: relative; margin-top: 12px; border-radius: 8px; overflow: hidden; background: #111; outline: 0 solid transparent; transition: outline-color .16s ease, box-shadow .16s ease; }
    .camera-wrap.scanned { outline: 5px solid #16a05d; box-shadow: 0 0 0 8px rgba(22, 160, 93, .22); }
    .camera-wrap.failed { outline: 5px solid #d64545; box-shadow: 0 0 0 8px rgba(214, 69, 69, .18); }
    video { display: block; width: 100%; max-height: 420px; background: #111; }
    .scan-badge { position: absolute; left: 50%; bottom: 16px; transform: translateX(-50%) translateY(12px); opacity: 0; pointer-events: none; padding: 10px 14px; border-radius: 999px; background: rgba(23, 32, 42, .86); color: white; font-weight: 750; transition: opacity .16s ease, transform .16s ease; }
    .scan-badge.visible { opacity: 1; transform: translateX(-50%) translateY(0); }
    button { border: 0; border-radius: 6px; padding: 10px 14px; font: inherit; font-weight: 650; cursor: pointer; background: #2457c5; color: white; }
    button.secondary { background: #e8edf4; color: #17202a; }
    button.positive { background: #197a4d; }
    button:disabled { opacity: .55; cursor: not-allowed; }
    input[type="file"] { display: none; }
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
      <button id="pickPhoto" class="secondary">Загрузить фото QR</button>
    </div>
    <input id="photoInput" type="file" accept="image/*">
    <div id="cameraWrap" class="camera-wrap">
      <video id="video" playsinline muted></video>
      <div id="scanBadge" class="scan-badge">QR считан</div>
    </div>
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
<script src="/scanner/assets/jsqr.js"></script>
<script>
const token = document.querySelector('#token');
const manual = document.querySelector('#manual');
const statusBox = document.querySelector('#status');
const card = document.querySelector('#card');
const confirmButton = document.querySelector('#confirmEntry');
const video = document.querySelector('#video');
const cameraWrap = document.querySelector('#cameraWrap');
const scanBadge = document.querySelector('#scanBadge');
const photoInput = document.querySelector('#photoInput');
let lastQR = '';
let stream = null;
let scanning = false;
let lastScanAt = 0;

token.value = new URLSearchParams(location.search).get('key') || localStorage.getItem('scannerToken') || '';
document.querySelector('#saveToken').onclick = () => {
  localStorage.setItem('scannerToken', token.value.trim());
  showStatus('Ключ сохранён.', true);
};
document.querySelector('#checkManual').onclick = () => verify(manual.value.trim());
confirmButton.onclick = () => confirmEntry();
document.querySelector('#startCamera').onclick = () => startCamera();
document.querySelector('#stopCamera').onclick = () => stopCamera();
document.querySelector('#pickPhoto').onclick = () => photoInput.click();
photoInput.onchange = () => scanPhoto(photoInput.files && photoInput.files[0]);

function showStatus(text, ok) {
  statusBox.textContent = text;
  statusBox.className = 'status ' + (ok ? 'ok' : 'bad');
}

function render(data) {
  confirmButton.disabled = !data.can_enter;
  card.innerHTML = '';
  if (!data.ok) {
    scanFeedback(false, data.message || 'QR не прошёл проверку.');
    showStatus(data.message || 'QR не прошёл проверку.', false);
    return;
  }
  scanFeedback(data.can_enter, data.can_enter ? 'QR считан' : 'QR считан, проход недоступен');
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
  if (qr === lastQR && Date.now() - lastScanAt < 2500) { return; }
  lastQR = qr;
  lastScanAt = Date.now();
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
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    showStatus('Браузер не дал доступ к камере. Откройте сканер по HTTPS или используйте ручной ввод.', false);
    return;
  }
  try {
    stream = await navigator.mediaDevices.getUserMedia({video: {facingMode: 'environment'}});
    video.srcObject = stream;
    await video.play();
    scanning = true;
    if ('BarcodeDetector' in window) {
      await scanWithBarcodeDetector();
    } else {
      await scanWithJSQR();
    }
  } catch (error) {
    showStatus('Не удалось включить камеру: ' + (error.message || error), false);
  }
}

async function scanWithBarcodeDetector() {
  const detector = new BarcodeDetector({formats: ['qr_code']});
  while (scanning) {
    const codes = await detector.detect(video).catch(() => []);
    if (codes.length > 0) {
      await verify(codes[0].rawValue);
      await wait(1600);
    }
    await nextFrame();
  }
}

async function scanWithJSQR() {
  if (typeof jsQR !== 'function') {
    showStatus('QR-движок не загрузился. Используйте ручной ввод.', false);
    return;
  }
  const canvas = document.createElement('canvas');
  const ctx = canvas.getContext('2d', {willReadFrequently: true});
  showStatus('Камера включена. Наведите её на QR-код.', true);
  while (scanning) {
    if (video.readyState >= HTMLMediaElement.HAVE_CURRENT_DATA) {
      canvas.width = video.videoWidth;
      canvas.height = video.videoHeight;
      ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
      const image = ctx.getImageData(0, 0, canvas.width, canvas.height);
      const code = jsQR(image.data, image.width, image.height, {inversionAttempts: 'dontInvert'});
      if (code && code.data) {
        await verify(code.data);
        await wait(1600);
      }
    }
    await nextFrame();
  }
}

async function scanPhoto(file) {
  if (!file) { return; }
  if (typeof jsQR !== 'function') {
    showStatus('QR-движок не загрузился. Используйте ручной ввод.', false);
    return;
  }
  const image = new Image();
  const objectUrl = URL.createObjectURL(file);
  image.onload = async () => {
    try {
      const canvas = document.createElement('canvas');
      const maxSide = 1600;
      const scale = Math.min(1, maxSide / Math.max(image.width, image.height));
      canvas.width = Math.max(1, Math.round(image.width * scale));
      canvas.height = Math.max(1, Math.round(image.height * scale));
      const ctx = canvas.getContext('2d', {willReadFrequently: true});
      ctx.drawImage(image, 0, 0, canvas.width, canvas.height);
      const frame = ctx.getImageData(0, 0, canvas.width, canvas.height);
      const code = jsQR(frame.data, frame.width, frame.height, {inversionAttempts: 'attemptBoth'});
      if (!code || !code.data) {
        scanFeedback(false, 'QR не найден на фото');
        showStatus('QR не найден на фото. Попробуйте другое изображение или ручной ввод.', false);
        return;
      }
      await verify(code.data);
    } finally {
      URL.revokeObjectURL(objectUrl);
      photoInput.value = '';
    }
  };
  image.onerror = () => {
    URL.revokeObjectURL(objectUrl);
    photoInput.value = '';
    showStatus('Не удалось открыть изображение.', false);
  };
  image.src = objectUrl;
}

function stopCamera() {
  scanning = false;
  cameraWrap.classList.remove('scanned', 'failed');
  scanBadge.classList.remove('visible');
  if (stream) {
    stream.getTracks().forEach(track => track.stop());
    stream = null;
  }
}

function scanFeedback(ok, text) {
  cameraWrap.classList.remove('scanned', 'failed');
  scanBadge.textContent = text;
  scanBadge.classList.add('visible');
  cameraWrap.classList.add(ok ? 'scanned' : 'failed');
  if (navigator.vibrate) {
    navigator.vibrate(ok ? [80, 40, 80] : [180]);
  }
  setTimeout(() => {
    cameraWrap.classList.remove('scanned', 'failed');
    scanBadge.classList.remove('visible');
  }, 1400);
}

function wait(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function nextFrame() {
  return new Promise(resolve => requestAnimationFrame(resolve));
}

function escapeHtml(value) {
  return String(value || '').replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
}
</script>
</body>
</html>`
