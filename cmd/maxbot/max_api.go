package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GetUpdates получает пачку событий MAX через long polling.
func (api *MaxAPI) GetUpdates(ctx context.Context, marker int64) (UpdateResponse, error) {
	query := url.Values{}
	query.Set("limit", "100")
	query.Set("timeout", "30")
	query.Set("types", "message_created,message_callback,bot_started")
	if marker > 0 {
		query.Set("marker", strconv.FormatInt(marker, 10))
	}

	var out UpdateResponse
	err := api.request(ctx, http.MethodGet, "/updates?"+query.Encode(), nil, &out)
	return out, err
}

// SendToUser отправляет обычное сообщение пользователю MAX.
func (api *MaxAPI) SendToUser(ctx context.Context, userID int64, text string, rows [][]Button) error {
	query := url.Values{}
	query.Set("user_id", strconv.FormatInt(userID, 10))
	return api.send(ctx, "/messages?"+query.Encode(), text, rows)
}

// SendFileToUser загружает файл и отправляет его пользователю.
func (api *MaxAPI) SendFileToUser(ctx context.Context, userID int64, text, fileName string, content []byte) error {
	info, err := api.UploadFile(ctx, fileName, content)
	if err != nil {
		return err
	}
	return api.SendUploadedFileToUser(ctx, userID, text, info)
}

// SendUploadedFileToUser отправляет пользователю уже загруженный в MAX файл.
func (api *MaxAPI) SendUploadedFileToUser(ctx context.Context, userID int64, text string, info UploadedInfo) error {
	query := url.Values{}
	query.Set("user_id", strconv.FormatInt(userID, 10))
	body := SendMessageBody{
		Text: text,
		Attachments: []Attachment{{
			Type:    "file",
			Payload: info,
		}},
	}
	var lastErr error
	delays := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for attempt, delay := range delays {
		// MAX может принять upload, но ещё не успеть подготовить файл для
		// отправки сообщением. Короткий retry делает экспорт надёжнее без
		// повторного формирования Excel.
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				if lastErr != nil {
					return lastErr
				}
				return ctx.Err()
			case <-timer.C:
			}
		}
		err := api.request(ctx, http.MethodPost, "/messages?"+query.Encode(), body, nil)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isAttachmentNotReady(err) {
			return err
		}
		if attempt < len(delays)-1 {
			log.Printf("max attachment is not ready, retrying file send in %s", delays[attempt+1])
		}
	}
	return lastErr
}

// UploadFile отправляет файл в MAX и возвращает данные вложения.
func (api *MaxAPI) UploadFile(ctx context.Context, fileName string, content []byte) (UploadedInfo, error) {
	query := url.Values{}
	query.Set("type", "file")
	var endpoint UploadEndpoint
	if err := api.request(ctx, http.MethodPost, "/uploads?"+query.Encode(), nil, &endpoint); err != nil {
		return UploadedInfo{}, err
	}
	if strings.TrimSpace(endpoint.URL) == "" {
		return UploadedInfo{}, errors.New("max api returned empty upload URL")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("data", filepath.Base(fileName))
	if err != nil {
		return UploadedInfo{}, err
	}
	if _, err := part.Write(content); err != nil {
		return UploadedInfo{}, err
	}
	if err := writer.Close(); err != nil {
		return UploadedInfo{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.URL, &body)
	if err != nil {
		return UploadedInfo{}, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := api.client.Do(req)
	if err != nil {
		return UploadedInfo{}, err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UploadedInfo{}, fmt.Errorf("max file upload: status %d: %s", resp.StatusCode, string(payload))
	}
	var info UploadedInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		return UploadedInfo{}, err
	}
	return info, nil
}

// isAttachmentNotReady понимает, что MAX еще не успел подготовить файл.
func isAttachmentNotReady(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "attachment.not.ready") || strings.Contains(text, "file.not.processed")
}

// AnswerCallback отвечает на нажатие inline-кнопки.
func (api *MaxAPI) AnswerCallback(ctx context.Context, callbackID, text string, rows [][]Button) error {
	query := url.Values{}
	query.Set("callback_id", callbackID)
	body := map[string]interface{}{
		"message": messageBody(text, rows),
	}
	return api.request(ctx, http.MethodPost, "/answers?"+query.Encode(), body, nil)
}

// send отправляет сообщение в MAX по готовому API-пути.
func (api *MaxAPI) send(ctx context.Context, path, text string, rows [][]Button) error {
	body := messageBody(text, rows)
	return api.request(ctx, http.MethodPost, path, body, nil)
}

// messageBody собирает текст, markdown и кнопки в формат MAX API.
func messageBody(text string, rows [][]Button) SendMessageBody {
	body := SendMessageBody{Text: text}
	if len(rows) > 0 {
		body.Attachments = []Attachment{{
			Type: "inline_keyboard",
			Payload: map[string]interface{}{
				"buttons": rows,
			},
		}}
	}
	return body
}

// request выполняет HTTP-запрос к MAX API и разбирает ответ.
func (api *MaxAPI) request(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, api.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", api.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := api.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("max api %s %s: status %d: %s", method, path, resp.StatusCode, string(payload))
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, out)
}
