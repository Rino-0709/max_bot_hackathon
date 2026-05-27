package main

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// displayName выбирает имя из профиля MAX, а если его нет — делает запасное.
func displayName(user MaxUser) string {
	if strings.TrimSpace(user.Name) != "" {
		return strings.TrimSpace(user.Name)
	}
	if user.Username != nil && strings.TrimSpace(*user.Username) != "" {
		return strings.TrimSpace(*user.Username)
	}
	return fmt.Sprintf("MAX %d", user.UserID)
}

// nowISO возвращает текущее время в формате для хранения.
func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// moscowLocation возвращает московскую временную зону проекта.
func moscowLocation() *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.FixedZone("Europe/Moscow", 3*60*60)
	}
	return loc
}

// moscowNow возвращает текущее время по Москве.
func moscowNow() time.Time {
	return time.Now().In(moscowLocation())
}

// todayMoscow возвращает сегодняшнюю дату по Москве.
func todayMoscow() string {
	return moscowNow().Format("2006-01-02")
}

// addDays прибавляет к дате нужное количество дней.
func addDays(date string, days int) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("2006-01-02")
}

// formatDate показывает дату в привычном виде ДД.ММ.ГГГГ.
func formatDate(date string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	return t.Format("02.01.2006")
}

// formatDateTime показывает дату и время из БД в читаемом виде.
func formatDateTime(value string) string {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return t.In(moscowLocation()).Format("02.01.2006 15:04")
}

// nullText возвращает строку из nullable-поля или пустое значение.
func nullText(value sql.NullString) string {
	if value.Valid && value.String != "" {
		return value.String
	}
	return "не указано"
}

// nullableIntArg готовит nullable-число для SQL.
func nullableIntArg(value sql.NullInt64) interface{} {
	if value.Valid {
		return value.Int64
	}
	return nil
}

// nullableStringArg готовит nullable-строку для SQL.
func nullableStringArg(value sql.NullString) interface{} {
	if value.Valid {
		return value.String
	}
	return nil
}

// validateFullName проверяет, что ФИО похоже на полное имя.
func validateFullName(value string) string {
	text := normalizeSpaces(value)
	if len([]rune(text)) < 5 {
		return "ФИО слишком короткое."
	}
	if regexp.MustCompile(`^\d+$`).MatchString(text) {
		return "ФИО не может состоять только из цифр."
	}
	if len(strings.Fields(text)) < 2 {
		return "Укажите минимум фамилию и имя."
	}
	return ""
}

// validatePurpose проверяет, что цель визита заполнена нормально.
func validatePurpose(value string) string {
	text := strings.TrimSpace(value)
	if len([]rune(text)) < 3 {
		return "Цель визита слишком короткая."
	}
	if len([]rune(text)) > 300 {
		return "Цель визита должна быть не длиннее 300 символов."
	}
	return ""
}

// validateDate проверяет дату на прошлое и слишком дальнее будущее.
func validateDate(value string) error {
	visitDate, err := time.ParseInLocation("2006-01-02", value, moscowLocation())
	if err != nil {
		return errors.New("Дата должна быть в формате ДД.ММ.ГГГГ.")
	}
	if value < todayMoscow() {
		return errors.New("Дата визита не может быть в прошлом.")
	}
	if !visitDate.Before(maxBookableDate()) {
		return errors.New("Сейчас слишком рано оформлять пропуск на эту дату. Выберите дату раньше чем через 2 месяца.")
	}
	return nil
}

// maxBookableDate считает границу бронирования на два месяца вперед.
func maxBookableDate() time.Time {
	now := moscowNow()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, moscowLocation()).AddDate(0, 2, 0)
}

// validateTime проверяет время в формате ЧЧ:ММ.
func validateTime(value string) string {
	if !regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`).MatchString(strings.TrimSpace(value)) {
		return "Время должно быть в формате ЧЧ:ММ."
	}
	return ""
}

// validateVisitDateTime проверяет, что выбранное время еще не прошло.
func validateVisitDateTime(visitDate, visitTime string) string {
	if err := validateDate(visitDate); err != nil {
		return err.Error()
	}
	if errText := validateTime(visitTime); errText != "" {
		return errText
	}
	loc, _ := time.LoadLocation("Europe/Moscow")
	visitAt, err := time.ParseInLocation("2006-01-02 15:04", visitDate+" "+strings.TrimSpace(visitTime), loc)
	if err != nil {
		return "Время должно быть в формате ЧЧ:ММ."
	}
	if visitAt.Before(moscowNow().Add(-1 * time.Hour)) {
		return "Это время уже прошло больше часа назад. Выберите другое время."
	}
	return ""
}

// validateDraftVisitTime проверяет время с датой из текущего черновика.
func (app *App) validateDraftVisitTime(userID int64, visitTime string) string {
	draft, err := app.getDraft(userID)
	if err != nil || draft == nil || !draft.VisitDate.Valid {
		return validateTime(visitTime)
	}
	return validateVisitDateTime(draft.VisitDate.String, visitTime)
}

// parseDateInput превращает ручной ввод даты в формат YYYY-MM-DD.
func parseDateInput(value string) string {
	text := strings.TrimSpace(value)
	if regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(text) {
		return text
	}
	match := regexp.MustCompile(`^(\d{2})\.(\d{2})\.(\d{4})$`).FindStringSubmatch(text)
	if len(match) == 4 {
		return match[3] + "-" + match[2] + "-" + match[1]
	}
	return ""
}

// normalizeSpaces схлопывает лишние пробелы в пользовательском вводе.
func normalizeSpaces(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

// statusLabel переводит внутренний статус заявки в русский текст.
func statusLabel(status string) string {
	labels := map[string]string{
		"pending_review":          "на рассмотрении",
		"clarification_requested": "ожидает уточнения",
		"approved":                "одобрена",
		"rejected":                "отклонена",
		"cancelled_by_initiator":  "отменена",
		"passed":                  "проходил",
		"no_show":                 "не пришёл",
		"expired":                 "истекла",
		"closed":                  "закрыта",
		"data_erasure_requested":  "данные удалены по запросу",
	}
	if label, ok := labels[status]; ok {
		return label
	}
	return status
}

// roleLabel переводит внутреннюю роль пользователя в русский текст.
func roleLabel(role string) string {
	labels := map[string]string{
		roleInitiator: "инициатор",
		roleAdmin:     "администратор",
		roleTechAdmin: "технический администратор",
	}
	if label, ok := labels[role]; ok {
		return label
	}
	return role
}

// entryLabel переводит тип прохода в понятную подпись.
func entryLabel(entryType string) string {
	if entryType == "re_entry" {
		return "повторный проход"
	}
	return "первый проход"
}

// actionLabel переводит код действия аудита в понятный текст.
func actionLabel(action string) string {
	labels := map[string]string{
		"admin_role_granted":     "выдал роль администратора",
		"admin_role_revoked":     "отозвал роль администратора",
		"consent_accepted":       "принял согласие",
		"pass_request_created":   "создал заявку",
		"request_approved":       "одобрил заявку",
		"request_rejected":       "отклонил заявку",
		"request_closed":         "закрыл заявку",
		"first_entry":            "подтвердил первый проход",
		"re_entry":               "подтвердил повторный проход",
		"zone_added":             "добавил зону",
		"zone_toggled":           "изменил активность зоны",
		"extra_fields_added":     "добавил доп. поля формы",
		"extra_field_toggled":    "изменил доп. поле формы",
		"clarification_answered": "ответил на уточнение",
	}
	if label, ok := labels[action]; ok {
		return label
	}
	return action
}

// placeholders собирает строку плейсхолдеров для SQL IN.
func placeholders(count int) string {
	items := make([]string, count)
	for i := range items {
		items[i] = "?"
	}
	return strings.Join(items, ",")
}

// contains проверяет наличие строки в небольшом списке.
func contains(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

// parseInt разбирает int и возвращает 0 при ошибке.
func parseInt(value string) int {
	result, _ := strconv.Atoi(value)
	if result < 0 {
		return 0
	}
	return result
}

// parseInt64 разбирает int64 и возвращает 0 при ошибке.
func parseInt64(value string) int64 {
	result, _ := strconv.ParseInt(value, 10, 64)
	return result
}

// looksLikeUserID проверяет, похож ли текст на MAX user id.
func looksLikeUserID(value string) bool {
	text := strings.TrimSpace(value)
	if len(text) < 4 || len(text) > 20 {
		return false
	}
	return regexp.MustCompile(`^\d+$`).MatchString(text)
}

// looksLikeRequestNumberQuery проверяет, похож ли текст на номер заявки.
func looksLikeRequestNumberQuery(value string) bool {
	text := strings.ToUpper(strings.TrimSpace(value))
	if strings.HasPrefix(text, "PASS-") {
		return true
	}
	return regexp.MustCompile(`^[A-Z0-9]{5}$`).MatchString(text)
}

// isMainMenuText узнает текстовые команды из главного меню.
func isMainMenuText(value string) bool {
	text := strings.ToLower(strings.TrimSpace(value))
	return text == "главное меню" || text == "меню" || text == "start"
}

// slug делает безопасный кусок имени файла.
func slug(value string) string {
	text := strings.ToLower(value)
	text = regexp.MustCompile(`[^a-zа-яё0-9]+`).ReplaceAllString(text, "-")
	text = strings.Trim(text, "-")
	if text == "" {
		return "zone-" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	return text
}
