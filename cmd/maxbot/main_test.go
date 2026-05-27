package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"
)

func newTestApp(t *testing.T) *App {
	t.Helper()

	if err := loadDotEnv("../../.env"); err != nil {
		t.Fatalf("load .env: %v", err)
	}
	cfg := loadConfig()
	cfg.DBDriver = "postgres"

	baseDB, err := openDatabase(cfg)
	if err != nil {
		t.Fatalf("open postgres db: %v", err)
	}
	schema := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := baseDB.Exec("CREATE SCHEMA " + quoteIdent(schema)); err != nil {
		_ = baseDB.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = baseDB.Exec("DROP SCHEMA " + quoteIdent(schema) + " CASCADE")
		_ = baseDB.Close()
	})

	cfg.DatabaseURL = withSearchPath(t, cfg.DatabaseURL, schema)
	db, err := openDatabase(cfg)
	if err != nil {
		t.Fatalf("open postgres test schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	app := &App{
		cfg: Config{
			DBDriver:      "postgres",
			DatabaseURL:   cfg.DatabaseURL,
			DataDir:       cfg.DataDir,
			PolicyVersion: "test-policy",
			AdminIDs:      map[int64]bool{},
			TechAdminIDs:  map[int64]bool{},
		},
		db:        db,
		lastStart: map[int64]time.Time{},
	}
	if err := app.initDB(); err != nil {
		t.Fatalf("init db: %v", err)
	}
	return app
}

func withSearchPath(t *testing.T, rawURL, schema string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	query := parsed.Query()
	query.Set("options", "-c search_path="+schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func quoteIdent(identifier string) string {
	if !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(identifier) {
		panic("unsafe postgres identifier: " + identifier)
	}
	return `"` + identifier + `"`
}

func testUser() MaxUser {
	return MaxUser{UserID: 1001, Name: "Тестовый пользователь"}
}

func upsertConsentedUser(t *testing.T, app *App) UserRow {
	t.Helper()
	user := upsertUser(t, app, testUser())
	consentUser(t, app, user)
	return user
}

func upsertUser(t *testing.T, app *App, maxUser MaxUser) UserRow {
	t.Helper()
	user, err := app.upsertUser(maxUser)
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	return user
}

func consentUser(t *testing.T, app *App, user UserRow) {
	t.Helper()
	_, err := app.exec(`
		INSERT INTO consents (user_id, document_version, scope, accepted_at)
		VALUES (?, ?, 'profile_and_pass_requests', ?)
	`, user.ID, app.cfg.PolicyVersion, nowISO())
	if err != nil {
		t.Fatalf("insert consent: %v", err)
	}
}

func testMaxUser(id int64, name string) MaxUser {
	return MaxUser{UserID: id, Name: name}
}

func createConsentedUser(t *testing.T, app *App, id int64, name string) UserRow {
	t.Helper()
	user := upsertUser(t, app, testMaxUser(id, name))
	consentUser(t, app, user)
	return user
}

func setUserRole(t *testing.T, app *App, maxUserID int64, role string) UserRow {
	t.Helper()
	if _, err := app.exec(`UPDATE users SET role = ? WHERE max_user_id = ?`, role, maxUserID); err != nil {
		t.Fatalf("set role: %v", err)
	}
	user, err := app.userByMaxID(maxUserID)
	if err != nil {
		t.Fatalf("get role user: %v", err)
	}
	if user == nil {
		t.Fatalf("user %d not found", maxUserID)
	}
	return *user
}

func createDraftRequest(t *testing.T, app *App, user UserRow, visitDate string, zoneID int64) string {
	t.Helper()
	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{
		"full_name":     "Иванов Иван Иванович",
		"visit_date":    visitDate,
		"visit_time":    safeVisitTime(visitDate),
		"zone_id":       zoneID,
		"visit_purpose": "Консультация в деканате",
	}); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	number, err := app.createPassRequest(user)
	if err != nil {
		t.Fatalf("create pass request: %v", err)
	}
	return number
}

func mustRequestByNumber(t *testing.T, app *App, number string) RequestRow {
	t.Helper()
	req, err := app.requestByNumber(number)
	if err != nil {
		t.Fatalf("request by number: %v", err)
	}
	if req == nil {
		t.Fatalf("request %s not found", number)
	}
	return *req
}

func requestStatus(t *testing.T, app *App, requestID int64) string {
	t.Helper()
	var status string
	if err := app.queryRow(`SELECT status FROM pass_requests WHERE id = ?`, requestID).Scan(&status); err != nil {
		t.Fatalf("request status: %v", err)
	}
	return status
}

func auditCount(t *testing.T, app *App, actorMaxUserID int64, action string) int {
	t.Helper()
	var count int
	if err := app.queryRow(`SELECT COUNT(*) FROM audit_log WHERE actor_max_user_id = ? AND action = ?`, actorMaxUserID, action).Scan(&count); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return count
}

func lastReply(t *testing.T, app *App) TestReply {
	t.Helper()
	if len(app.testReplies) == 0 {
		t.Fatal("expected bot reply")
	}
	return app.testReplies[len(app.testReplies)-1]
}

func hasButtonText(rows [][]Button, text string) bool {
	for _, row := range rows {
		for _, button := range row {
			if button.Text == text {
				return true
			}
		}
	}
	return false
}

func hasButtonPayloadPrefix(rows [][]Button, prefix string) bool {
	for _, row := range rows {
		for _, button := range row {
			if strings.HasPrefix(button.Payload, prefix) {
				return true
			}
		}
	}
	return false
}

type BotScenario struct {
	t    *testing.T
	app  *App
	ctx  context.Context
	user MaxUser
}

func NewScenario(t *testing.T, app *App, user MaxUser) *BotScenario {
	t.Helper()
	return &BotScenario{t: t, app: app, ctx: context.Background(), user: user}
}

func (s *BotScenario) As(user MaxUser) *BotScenario {
	s.t.Helper()
	s.user = user
	return s
}

func (s *BotScenario) Say(text string) *BotScenario {
	s.t.Helper()
	if err := s.app.handleBotContext(s.ctx, BotContext{User: s.user, Text: text}); err != nil {
		s.t.Fatalf("say %q: %v", text, err)
	}
	return s
}

func (s *BotScenario) Command(text string) *BotScenario {
	s.t.Helper()
	return s.Say(text)
}

func (s *BotScenario) Click(text string) *BotScenario {
	s.t.Helper()
	reply := lastReply(s.t, s.app)
	for _, row := range reply.Rows {
		for _, button := range row {
			if button.Text == text {
				if err := s.app.handleBotContext(s.ctx, BotContext{User: s.user, Payload: button.Payload}); err != nil {
					s.t.Fatalf("click %q payload %q: %v", text, button.Payload, err)
				}
				return s
			}
		}
	}
	s.t.Fatalf("button %q not found in rows %#v; last text: %q", text, reply.Rows, reply.Text)
	return s
}

func (s *BotScenario) ClickPayload(payload string) *BotScenario {
	s.t.Helper()
	if err := s.app.handleBotContext(s.ctx, BotContext{User: s.user, Payload: payload}); err != nil {
		s.t.Fatalf("click payload %q: %v", payload, err)
	}
	return s
}

func (s *BotScenario) ExpectText(part string) *BotScenario {
	s.t.Helper()
	reply := lastReply(s.t, s.app)
	if !strings.Contains(reply.Text, part) {
		s.t.Fatalf("expected text containing %q, got %q", part, reply.Text)
	}
	return s
}

func (s *BotScenario) ExpectButton(text string) *BotScenario {
	s.t.Helper()
	reply := lastReply(s.t, s.app)
	if !hasButtonText(reply.Rows, text) {
		s.t.Fatalf("expected button %q, got rows %#v; text: %q", text, reply.Rows, reply.Text)
	}
	return s
}

func (s *BotScenario) ExpectNoButton(text string) *BotScenario {
	s.t.Helper()
	reply := lastReply(s.t, s.app)
	if hasButtonText(reply.Rows, text) {
		s.t.Fatalf("did not expect button %q, got rows %#v; text: %q", text, reply.Rows, reply.Text)
	}
	return s
}

func (s *BotScenario) ExpectNoText(part string) *BotScenario {
	s.t.Helper()
	reply := lastReply(s.t, s.app)
	if strings.Contains(reply.Text, part) {
		s.t.Fatalf("did not expect text containing %q, got %q", part, reply.Text)
	}
	return s
}

func (s *BotScenario) LastReply() TestReply {
	s.t.Helper()
	return lastReply(s.t, s.app)
}

func TestAskDateUsesHumanLabels(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	err := app.askDate(context.Background(), BotContext{User: testUser()}, user)
	if err != nil {
		t.Fatalf("ask date: %v", err)
	}

	reply := lastReply(t, app)
	if !hasButtonText(reply.Rows, "Послезавтра") {
		t.Fatalf("expected Послезавтра button, got %#v", reply.Rows)
	}
	if hasButtonText(reply.Rows, addDays(todayMoscow(), 2)) {
		t.Fatalf("did not expect raw ISO date as button label")
	}
	if !hasButtonText(reply.Rows, "Назад") || !hasButtonText(reply.Rows, "Главное меню") {
		t.Fatalf("expected back and main menu buttons, got %#v", reply.Rows)
	}
}

func TestDuplicateStartFromMaxEventsIsSuppressed(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	maxUser := testMaxUser(user.MaxUserID, user.DisplayName)

	if err := app.handleUpdate(context.Background(), Update{UpdateType: "bot_started", User: &maxUser}); err != nil {
		t.Fatalf("bot_started: %v", err)
	}
	msg := &Message{Sender: &maxUser}
	msg.Body.Text = "/start"
	if err := app.handleUpdate(context.Background(), Update{UpdateType: "message_created", Message: msg}); err != nil {
		t.Fatalf("message_created /start: %v", err)
	}

	if len(app.testReplies) != 1 {
		t.Fatalf("expected one start menu after duplicate events, got %d replies: %#v", len(app.testReplies), app.testReplies)
	}
	if !strings.Contains(app.testReplies[0].Text, "Главное меню") {
		t.Fatalf("expected main menu, got %q", app.testReplies[0].Text)
	}
}

func TestCustomDateInputMovesToTimeSelection(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	app.setSession(user.MaxUserID, "draft_date_custom", nil)

	input := todayMoscowAsHuman()
	err := app.handleBotContext(context.Background(), BotContext{User: testUser(), Text: input})
	if err != nil {
		t.Fatalf("handle custom date: %v", err)
	}

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.VisitDate.Valid || draft.VisitDate.String != todayMoscow() {
		t.Fatalf("expected visit date %s, got %#v", todayMoscow(), draft)
	}

	reply := lastReply(t, app)
	if !hasButtonPayloadPrefix(reply.Rows, "draft:time:") {
		t.Fatalf("expected time buttons after custom date, got %#v", reply.Rows)
	}
}

func TestRecoverCustomDateInputWithoutSession(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{"full_name": "Иванов Иван Иванович"}); err != nil {
		t.Fatalf("set full name: %v", err)
	}

	NewScenario(t, app, testUser()).
		Say(todayMoscowAsHuman()).
		ExpectText("Во сколько планируете прийти").
		ExpectNoText("Откройте меню командой /start")

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.VisitDate.Valid || draft.VisitDate.String != todayMoscow() {
		t.Fatalf("expected recovered custom date, got %#v", draft)
	}
}

func TestPastDateIsRejected(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	app.setSession(user.MaxUserID, "draft_date_custom", nil)

	NewScenario(t, app, testUser()).
		Say(formatDate(addDays(todayMoscow(), -1))).
		ExpectText("Дата визита не может быть в прошлом").
		ExpectButton("Назад").
		ExpectButton("Главное меню")
}

func TestCustomDateInputAcceptsHackathonDateWhileCurrent(t *testing.T) {
	if "2026-05-24" < todayMoscow() {
		t.Skip("24.05.2026 is already in the past for this test run")
	}

	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	app.setSession(user.MaxUserID, "draft_date_custom", nil)

	err := app.handleBotContext(context.Background(), BotContext{User: testUser(), Text: "24.05.2026"})
	if err != nil {
		t.Fatalf("handle hackathon date: %v", err)
	}

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.VisitDate.Valid || draft.VisitDate.String != "2026-05-24" {
		t.Fatalf("expected 2026-05-24, got %#v", draft)
	}
	if !hasButtonPayloadPrefix(lastReply(t, app).Rows, "draft:time:") {
		t.Fatalf("expected time selection after 24.05.2026")
	}
}

func TestDraftFlowRequestsAllFieldsInOrder(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	ctx := context.Background()
	maxUser := testUser()

	steps := []BotContext{
		{User: maxUser, Payload: "draft:start"},
		{User: maxUser, Text: "Иванов Иван Иванович"},
		{User: maxUser, Payload: "draft:date:" + todayMoscow()},
		{User: maxUser, Payload: "draft:time:" + safeVisitTime(todayMoscow())},
		{User: maxUser, Payload: "draft:zone:1"},
		{User: maxUser, Text: "Консультация в деканате"},
	}

	for i, step := range steps {
		if err := app.handleBotContext(ctx, step); err != nil {
			t.Fatalf("step %d failed: %v", i+1, err)
		}
	}

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil {
		t.Fatal("expected draft")
	}
	if !draft.FullName.Valid || draft.FullName.String != "Иванов Иван Иванович" {
		t.Fatalf("unexpected full name: %#v", draft.FullName)
	}
	if !draft.VisitDate.Valid || draft.VisitDate.String != todayMoscow() {
		t.Fatalf("unexpected date: %#v", draft.VisitDate)
	}
	if !draft.VisitTime.Valid || draft.VisitTime.String != safeVisitTime(todayMoscow()) {
		t.Fatalf("unexpected time: %#v", draft.VisitTime)
	}
	if !draft.ZoneID.Valid || draft.ZoneID.Int64 != 1 {
		t.Fatalf("unexpected zone: %#v", draft.ZoneID)
	}
	if !draft.VisitPurpose.Valid || draft.VisitPurpose.String != "Консультация в деканате" {
		t.Fatalf("unexpected purpose: %#v", draft.VisitPurpose)
	}

	reply := lastReply(t, app)
	if !strings.Contains(reply.Text, "Проверьте заявку") {
		t.Fatalf("expected summary screen, got %q", reply.Text)
	}
	if !hasButtonText(reply.Rows, "Отправить на рассмотрение") {
		t.Fatalf("expected submit button, got %#v", reply.Rows)
	}
}

func TestRecoverDraftInputWithoutSession(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}

	err := app.handleBotContext(context.Background(), BotContext{User: testUser(), Text: "Петров Петр Петрович"})
	if err != nil {
		t.Fatalf("recover draft input: %v", err)
	}

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.FullName.Valid || draft.FullName.String != "Петров Петр Петрович" {
		t.Fatalf("expected recovered full name, got %#v", draft)
	}
}

func TestScenarioInitiatorCreatesPassWithManualDate(t *testing.T) {
	app := newTestApp(t)
	user := testMaxUser(7001, "Сценарный Пользователь")
	s := NewScenario(t, app, user)

	s.Command("/start").
		ExpectText("Весенний_код_1").
		ExpectButton("Согласен").
		Click("Согласен").
		ExpectButton("Создать пропуск").
		Click("Создать пропуск").
		ExpectText("Введите ФИО полностью").
		Say("Сидоров Семен Сергеевич").
		ExpectText("Выберите дату посещения").
		ExpectButton("Послезавтра").
		ExpectButton("Другая дата").
		Click("Другая дата").
		ExpectText("Введите дату").
		Say(todayMoscowAsHuman()).
		ExpectText("Во сколько планируете прийти").
		ExpectButton("09:00").
		Click("Другое время").
		Say(safeVisitTime(todayMoscow())).
		ExpectText("Выберите корпус/зону").
		Click("Проспект Вернадского, 78").
		ExpectText("Кратко опишите цель").
		Say("Консультация в деканате").
		ExpectText("Проверьте заявку").
		ExpectButton("Отправить на рассмотрение").
		Click("Отправить на рассмотрение").
		ExpectText("Заявка отправлена на рассмотрение").
		ExpectButton("Мои заявки").
		Click("Мои заявки").
		ExpectText("Показаны последние 10 заявок").
		ExpectButton("Назад")
}

func TestScenarioFastFullNameAfterCreatePassIsCaptured(t *testing.T) {
	app := newTestApp(t)
	user := testMaxUser(7301, "Быстрый Пользователь")
	NewScenario(t, app, user).Command("/start").Click("Согласен")

	updates := []Update{
		{UpdateType: "message_callback", Callback: &Callback{User: user, Payload: "draft:start"}},
		{UpdateType: "message_created", Message: &Message{Sender: &user, Body: struct {
			MID  string `json:"mid"`
			Text string `json:"text"`
		}{Text: "Быстрый Иван Иванович"}}},
	}
	for _, group := range groupUpdatesByUser(updates) {
		for _, update := range group {
			if err := app.handleUpdate(context.Background(), update); err != nil {
				t.Fatalf("handle grouped update: %v", err)
			}
		}
	}

	reply := lastReply(t, app)
	if strings.Contains(reply.Text, "Откройте меню") {
		t.Fatalf("full name was not captured, got %q", reply.Text)
	}
	draft, err := app.getDraft(upsertUser(t, app, user).ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.FullName.Valid {
		t.Fatalf("expected full name in draft, got %#v", draft)
	}
}

func TestScenarioEditSingleDraftFieldReturnsToSummary(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{
		"full_name":     "Иванов Иван Иванович",
		"visit_date":    addDays(todayMoscow(), 1),
		"visit_time":    "09:00",
		"zone_id":       int64(1),
		"visit_purpose": "Консультация",
	}); err != nil {
		t.Fatalf("fill draft: %v", err)
	}

	s := NewScenario(t, app, testUser())
	s.ClickPayload("draft:edit").
		Click("Время").
		ExpectText("Во сколько планируете прийти").
		ExpectButton("Назад").
		ExpectButton("Главное меню").
		Click("11:00").
		ExpectText("Проверьте заявку").
		ExpectNoText("Выберите корпус/зону").
		ExpectNoButton("Проспект Вернадского, 78")

	draft, err := app.getDraft(user.ID)
	if err != nil {
		t.Fatalf("get draft: %v", err)
	}
	if draft == nil || !draft.VisitTime.Valid || draft.VisitTime.String != "11:00" {
		t.Fatalf("expected changed time only, got %#v", draft)
	}

	s.Click("Изменить").
		Click("Дата").
		Click("Завтра").
		ExpectText("Проверьте заявку").
		ExpectNoText("Во сколько планируете прийти")

	s.Click("Изменить").
		Click("Корпус/зона").
		Click("Проспект Вернадского, 86").
		ExpectText("Проверьте заявку").
		ExpectNoText("Кратко опишите цель")

	s.Click("Изменить").
		Click("Цель").
		Say("Новая цель визита").
		ExpectText("Проверьте заявку")

	s.Click("Изменить").
		Click("ФИО").
		Say("Петров Петр Петрович").
		ExpectText("Проверьте заявку").
		ExpectNoText("Выберите дату посещения")
}

func TestScenarioAdminReviewsClarifiesApprovesAndConfirmsEntry(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 7101, "Сценарный Админ").MaxUserID, roleAdmin)
	number := createDraftRequest(t, app, initiator, todayMoscow(), 1)
	req := mustRequestByNumber(t, app, number)
	adminUser := testMaxUser(admin.MaxUserID, admin.DisplayName)
	initiatorUser := testUser()

	NewScenario(t, app, adminUser).
		Command("/start").
		ExpectButton("Меню админа").
		Click("Меню админа").
		ExpectButton("Очередь").
		Click("Очередь").
		ExpectText(number).
		Click(number).
		ExpectText("Статус: на рассмотрении").
		ExpectButton("Запросить уточнение").
		Click("Запросить уточнение").
		ExpectText("Напишите вопросы").
		Say("Уточните цель визита").
		ExpectText("Уточнение запрошено")

	if got := requestStatus(t, app, req.ID); got != "clarification_requested" {
		t.Fatalf("expected clarification_requested, got %s", got)
	}

	NewScenario(t, app, initiatorUser).
		ClickPayload(fmt.Sprintf("request:answer_clarification:%d", req.ID)).
		ExpectText("Напишите ответ").
		Say("Иду на консультацию").
		ExpectText("Ответ сохранён")

	NewScenario(t, app, adminUser).
		ClickPayload(fmt.Sprintf("request:open:%d", req.ID)).
		ExpectText("Уточнение:").
		ExpectText("Вопрос: Уточните цель визита").
		ExpectText("Ответ: Иду на консультацию")

	NewScenario(t, app, adminUser).
		ClickPayload(fmt.Sprintf("admin:approve:%d", req.ID)).
		ExpectText("Одобрить заявку").
		Click("Одобрить без комментария").
		ExpectText("Заявка одобрена").
		ClickPayload(fmt.Sprintf("admin:entry:%d", req.ID)).
		ExpectText("Первый проход подтверждён").
		ClickPayload(fmt.Sprintf("admin:entry:%d", req.ID)).
		ExpectText("Повторный проход подтверждён")

	if got := requestStatus(t, app, req.ID); got != "passed" {
		t.Fatalf("expected passed, got %s", got)
	}
	if count := app.entryCount(req.ID); count != 2 {
		t.Fatalf("expected 2 entries, got %d", count)
	}
}

func TestScenarioAdminSearchCapturesRequestNumber(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 7151, "Поисковый админ").MaxUserID, roleAdmin)
	number := createDraftRequest(t, app, initiator, todayMoscow(), 1)
	adminUser := testMaxUser(admin.MaxUserID, admin.DisplayName)

	NewScenario(t, app, adminUser).
		Command("/start").
		Click("Меню админа").
		Click("Поиск по номеру").
		ExpectText("Введите номер заявки").
		ExpectButton("Меню админа").
		ExpectButton("Главное меню").
		Say(number).
		ExpectText("Заявка: " + number).
		ExpectNoText("Откройте меню командой /start")

	shortNumber := number[len(number)-5:]
	NewScenario(t, app, adminUser).
		ClickPayload("admin:search").
		ExpectText("Введите номер заявки").
		Say(shortNumber).
		ExpectText("Заявка: " + number).
		ExpectNoText("Откройте меню командой /start")

	NewScenario(t, app, adminUser).
		Say(shortNumber).
		ExpectText("Заявка: " + number).
		ExpectNoText("Откройте меню командой /start")
}

func TestScenarioTechAdminManagesAdmins(t *testing.T) {
	app := newTestApp(t)
	tech := setUserRole(t, app, createConsentedUser(t, app, 7201, "Сценарный Тех").MaxUserID, roleTechAdmin)
	target := createConsentedUser(t, app, 7202, "Сценарный Админ")
	techUser := testMaxUser(tech.MaxUserID, tech.DisplayName)

	NewScenario(t, app, techUser).
		Command("/start").
		ExpectButton("Меню тех админа").
		Click("Меню тех админа").
		ExpectButton("Обычные админы").
		ExpectButton("Тех админы").
		ExpectNoButton("Добавить админа").
		Click("Обычные админы").
		ExpectButton("Добавить админа").
		Click("Добавить админа").
		ExpectText("Введите MAX user id").
		Say(fmt.Sprint(target.MaxUserID)).
		ExpectText("Роль администратора выдана").
		Click("Меню тех админа").
		Click("Обычные админы").
		ExpectButton(target.DisplayName).
		Click(target.DisplayName).
		ExpectButton("Действия").
		ExpectButton("Забрать роль админа").
		Click("Забрать роль админа").
		ExpectText("Роль администратора отозвана")

	updated, err := app.userByMaxID(target.MaxUserID)
	if err != nil {
		t.Fatalf("get updated target: %v", err)
	}
	if updated == nil || updated.Role != roleInitiator {
		t.Fatalf("expected initiator after revoke, got %#v", updated)
	}
}

func TestOnboardingConsentAndRoleMenus(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	maxUser := testMaxUser(2001, "Новый пользователь")

	if err := app.handleBotContext(ctx, BotContext{User: maxUser, Text: "/start"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "Весенний_код_1") {
		t.Fatalf("expected consent screen, got %q", lastReply(t, app).Text)
	}
	if !strings.Contains(lastReply(t, app).Text, "паспортные данные") {
		t.Fatalf("expected personal data disclaimer, got %q", lastReply(t, app).Text)
	}
	if !strings.Contains(lastReply(t, app).Text, "Нажимая «Согласен»") {
		t.Fatalf("expected explicit consent wording, got %q", lastReply(t, app).Text)
	}
	if !hasButtonText(lastReply(t, app).Rows, "Политика данных") {
		t.Fatalf("expected data policy button")
	}

	if err := app.handleBotContext(ctx, BotContext{User: maxUser, Payload: "consent:accept"}); err != nil {
		t.Fatalf("accept consent: %v", err)
	}
	if !hasButtonText(lastReply(t, app).Rows, "Создать пропуск") {
		t.Fatalf("expected main menu after consent")
	}
	if !hasButtonText(lastReply(t, app).Rows, "Политика и согласие") {
		t.Fatalf("expected policy button in main menu")
	}

	user, err := app.userByMaxID(maxUser.UserID)
	if err != nil || user == nil {
		t.Fatalf("get user: %v", err)
	}
	admin := setUserRole(t, app, maxUser.UserID, roleAdmin)
	if !hasButtonText(app.mainMenu(admin), "Меню админа") {
		t.Fatalf("expected admin menu button")
	}
	tech := setUserRole(t, app, maxUser.UserID, roleTechAdmin)
	if !hasButtonText(app.mainMenu(tech), "Меню тех админа") {
		t.Fatalf("expected tech admin menu button")
	}
}

func TestDataPolicyAndConsentWithdrawal(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	number := createDraftRequest(t, app, user, todayMoscow(), 1)

	NewScenario(t, app, testUser()).
		ClickPayload("data:policy").
		ExpectText("Политика данных").
		ExpectButton("Отозвать согласие").
		Click("Отозвать согласие").
		ExpectText("Отозвать согласие и запросить удаление данных").
		ExpectButton("Да, отозвать").
		Click("Да, отозвать").
		ExpectText("Согласие отозвано")

	if app.hasConsent(user.ID) {
		t.Fatalf("expected consent to be withdrawn")
	}
	req := mustRequestByNumber(t, app, number)
	if req.FullName != "Удалено по запросу пользователя" {
		t.Fatalf("expected anonymized full name, got %q", req.FullName)
	}
	if got := requestStatus(t, app, req.ID); got != "data_erasure_requested" {
		t.Fatalf("expected data_erasure_requested, got %s", got)
	}
}

func TestMyRequestsEmptyHasBackButton(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)

	NewScenario(t, app, testUser()).
		ClickPayload("my:list").
		ExpectText("У вас пока нет заявок").
		ExpectButton("Создать пропуск").
		ExpectButton("Назад").
		ExpectButton("Главное меню")

	_ = user
}

func TestMyRequestsListEndsWithBackButton(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	_ = createDraftRequest(t, app, user, todayMoscow(), 1)

	NewScenario(t, app, testUser()).
		ClickPayload("my:list").
		ExpectText("Показаны последние 10 заявок").
		ExpectButton("Назад").
		ExpectButton("Главное меню")
}

func TestClarificationNotificationLetsInitiatorAnswer(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 5101, "Администратор").MaxUserID, roleAdmin)
	req := mustRequestByNumber(t, app, createDraftRequest(t, app, initiator, todayMoscow(), 1))

	app.setSession(admin.MaxUserID, "admin_clarify", map[string]string{"request_id": fmt.Sprint(req.ID)})
	if err := app.handleBotContext(context.Background(), BotContext{
		User: testMaxUser(admin.MaxUserID, admin.DisplayName),
		Text: "вы уверены?",
	}); err != nil {
		t.Fatalf("request clarification: %v", err)
	}

	if len(app.testReplies) < 2 {
		t.Fatalf("expected owner notification and admin reply, got %#v", app.testReplies)
	}
	notification := app.testReplies[len(app.testReplies)-2]
	if !strings.Contains(notification.Text, "вы уверены?") {
		t.Fatalf("expected clarification question in notification, got %q", notification.Text)
	}
	if !hasButtonText(notification.Rows, "Ответить на уточнение") {
		t.Fatalf("expected answer button in notification, got %#v", notification.Rows)
	}

	NewScenario(t, app, testUser()).
		ClickPayload(fmt.Sprintf("request:answer_clarification:%d", req.ID)).
		ExpectText("Напишите ответ на уточнение").
		ExpectButton("Назад").
		Say("Да, подтверждаю.").
		ExpectText("Ответ сохран")

	if got := requestStatus(t, app, req.ID); got != "pending_review" {
		t.Fatalf("expected request to return to pending_review, got %s", got)
	}
}

func TestValidationHelpers(t *testing.T) {
	if validateFullName("12345") == "" {
		t.Fatalf("numeric full name must be invalid")
	}
	if validateFullName("Иванов Иван") != "" {
		t.Fatalf("surname and name should be valid")
	}
	if parseDateInput("24.05.2026") != "2026-05-24" {
		t.Fatalf("unexpected date parse")
	}
	if parseDateInput("2026-05-24") != "2026-05-24" {
		t.Fatalf("unexpected ISO date parse")
	}
	if validateTime("25:00") == "" {
		t.Fatalf("25:00 must be invalid")
	}
	if validateTime("09:30") != "" {
		t.Fatalf("09:30 should be valid")
	}
	if rebindPostgres("SELECT '?' AS literal, a = ? AND b = ?") != "SELECT '?' AS literal, a = $1 AND b = $2" {
		t.Fatalf("postgres rebind must skip placeholders inside string literals")
	}
	old := moscowNow().Add(-2 * time.Hour)
	if validateVisitDateTime(old.Format("2006-01-02"), old.Format("15:04")) == "" {
		t.Fatalf("visit time older than one hour must be rejected")
	}
	future := moscowNow().Add(2 * time.Hour)
	if validateVisitDateTime(future.Format("2006-01-02"), future.Format("15:04")) != "" {
		t.Fatalf("future visit time should be valid")
	}
	if err := validateDate(maxBookableDate().Format("2006-01-02")); err == nil {
		t.Fatalf("date exactly two months ahead must be rejected")
	}
	if got := moscowNow().Location(); got == nil {
		t.Fatalf("moscow location fallback must never be nil")
	}
	scope, action, id, extra := parsePayload("admin:reject_reason:42:Недостаточно данных")
	if scope != "admin" || action != "reject_reason" || id != "42" || extra != "Недостаточно данных" {
		t.Fatalf("unexpected reject payload parse: %q %q %q %q", scope, action, id, extra)
	}
	_, action, id, _ = parsePayload("draft:time:09:00")
	if action != "time" || id != "09:00" {
		t.Fatalf("unexpected time payload parse: action=%q id=%q", action, id)
	}
}

func TestPostgresOpenAndInit(t *testing.T) {
	app := newTestApp(t)
	var ok int
	if err := app.queryRow(`SELECT 1 FROM users LIMIT 1`).Scan(&ok); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("query postgres users: %v", err)
	}
}

func TestLoadDotEnvAcceptsUTF8BOM(t *testing.T) {
	t.Setenv("BOT_TOKEN", "")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("\xef\xbb\xbfBOT_TOKEN=test-token\n"), 0600); err != nil {
		t.Fatalf("write bom env: %v", err)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatalf("load bom env: %v", err)
	}
	if got := os.Getenv("BOT_TOKEN"); got != "test-token" {
		t.Fatalf("expected token from BOM env, got %q", got)
	}
}

func TestMonitoringHealthAndMetrics(t *testing.T) {
	app := newTestApp(t)
	app.metrics = newMetrics()
	app.metrics.incUpdate("message_created")
	app.metrics.repliesTotal.Add(2)
	user := upsertConsentedUser(t, app)
	createDraftRequest(t, app, user, todayMoscow(), 1)

	health := httptest.NewRecorder()
	app.healthHandler(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), "ok") {
		t.Fatalf("unexpected health response: %d %q", health.Code, health.Body.String())
	}

	metrics := httptest.NewRecorder()
	app.metricsHandler(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := metrics.Body.String()
	if metrics.Code != http.StatusOK {
		t.Fatalf("unexpected metrics status: %d %q", metrics.Code, body)
	}
	for _, expected := range []string{
		"maxbot_build_info",
		`type="message_created"`,
		`type_label="сообщения пользователей"`,
		"maxbot_replies_total 2",
		"maxbot_last_poll_duration_seconds",
		"maxbot_last_update_batch_size",
		"maxbot_last_update_lag_seconds",
		`status="pending_review"`,
		`status_label="на рассмотрении"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected metrics to contain %q, got:\n%s", expected, body)
		}
	}
}

func TestUpdateLagParsesMaxTimestamp(t *testing.T) {
	updateTime := time.Now().Add(-2 * time.Second)
	lag, ok := updateLag(Update{Timestamp: updateTime.UnixMilli()})
	if !ok {
		t.Fatalf("expected update lag")
	}
	if lag < time.Second || lag > 5*time.Second {
		t.Fatalf("unexpected update lag: %s", lag)
	}
}

func TestAttachmentNotReadyErrorIsRetryable(t *testing.T) {
	err := errors.New(`max api POST /messages: status 400: {"code":"attachment.not.ready","message":"Key: errors.process.attachment.file.not.processed"}`)
	if !isAttachmentNotReady(err) {
		t.Fatalf("expected attachment.not.ready to be retryable")
	}
	if isAttachmentNotReady(errors.New("other max api error")) {
		t.Fatalf("unexpected retryable error")
	}
}

func TestSubmitRequestAndBlockDuplicateSameDateZone(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	number := createDraftRequest(t, app, user, todayMoscow(), 1)
	req := mustRequestByNumber(t, app, number)
	if req.Status != "pending_review" {
		t.Fatalf("expected pending review, got %s", req.Status)
	}
	if _, err := app.createPassRequest(user); err == nil {
		t.Fatalf("expected incomplete draft error after submit")
	}

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure duplicate draft: %v", err)
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{
		"full_name":     "Иванов Иван Иванович",
		"visit_date":    todayMoscow(),
		"visit_time":    safeVisitTime(todayMoscow()),
		"zone_id":       int64(1),
		"visit_purpose": "Повторная консультация",
	}); err != nil {
		t.Fatalf("duplicate draft: %v", err)
	}
	if _, err := app.createPassRequest(user); err == nil {
		t.Fatalf("expected duplicate request error")
	}
}

func TestAllowsSameDateDifferentZone(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	createDraftRequest(t, app, user, todayMoscow(), 1)
	createDraftRequest(t, app, user, todayMoscow(), 2)
}

func TestInitiatorCanCancelPendingRequest(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	req := mustRequestByNumber(t, app, createDraftRequest(t, app, user, todayMoscow(), 1))

	err := app.handleBotContext(context.Background(), BotContext{
		User:    testUser(),
		Payload: fmt.Sprintf("request:cancel:%d", req.ID),
	})
	if err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	if got := requestStatus(t, app, req.ID); got != "cancelled_by_initiator" {
		t.Fatalf("expected cancelled status, got %s", got)
	}
}

func TestStatusTransitionsAreValidatedAndClosedAtIsSet(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 2901, "Админ переходов").MaxUserID, roleAdmin)
	req := mustRequestByNumber(t, app, createDraftRequest(t, app, user, todayMoscow(), 1))

	if err := app.updateRequestStatus(req.ID, "approved", admin, "Одобрено.", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := app.updateRequestStatus(req.ID, "rejected", admin, "Отклонено.", "Причина"); err == nil {
		t.Fatalf("approved request must not be rejected by stale callback")
	}
	if err := app.updateRequestStatus(req.ID, "closed", admin, "Закрыто.", ""); err != nil {
		t.Fatalf("close: %v", err)
	}

	var closedAt sql.NullString
	if err := app.queryRow(`SELECT closed_at FROM pass_requests WHERE id = ?`, req.ID).Scan(&closedAt); err != nil {
		t.Fatalf("closed_at query: %v", err)
	}
	if !closedAt.Valid || strings.TrimSpace(closedAt.String) == "" {
		t.Fatalf("expected closed_at to be set")
	}
}

func TestAdminApproveRejectClarifyAndAnswer(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 3001, "Админ").MaxUserID, roleAdmin)

	approved := mustRequestByNumber(t, app, createDraftRequest(t, app, initiator, todayMoscow(), 1))
	adminCtx := testMaxUser(admin.MaxUserID, admin.DisplayName)
	if err := app.handleBotContext(context.Background(), BotContext{User: adminCtx, Payload: fmt.Sprintf("admin:approve:%d", approved.ID)}); err != nil {
		t.Fatalf("approve menu: %v", err)
	}
	if !hasButtonText(lastReply(t, app).Rows, "Одобрить без комментария") {
		t.Fatalf("expected approve confirmation buttons")
	}
	if err := app.handleBotContext(context.Background(), BotContext{User: adminCtx, Payload: fmt.Sprintf("admin:approve_now:%d", approved.ID)}); err != nil {
		t.Fatalf("approve now: %v", err)
	}
	if got := requestStatus(t, app, approved.ID); got != "approved" {
		t.Fatalf("expected approved, got %s", got)
	}

	withComment := mustRequestByNumber(t, app, createDraftRequest(t, app, initiator, addDays(todayMoscow(), 1), 1))
	app.setSession(admin.MaxUserID, "admin_approve_comment", map[string]string{"request_id": fmt.Sprint(withComment.ID)})
	if err := app.handleBotContext(context.Background(), BotContext{User: adminCtx, Text: "Вход через пост охраны №1"}); err != nil {
		t.Fatalf("approve with comment: %v", err)
	}
	approvedWithComment := mustRequestByNumber(t, app, withComment.RequestNumber)
	if !approvedWithComment.PublicComment.Valid || approvedWithComment.PublicComment.String != "Вход через пост охраны №1" {
		t.Fatalf("expected approval comment, got %#v", approvedWithComment.PublicComment)
	}

	otherUser := createConsentedUser(t, app, 3002, "Другой пользователь")
	rejected := mustRequestByNumber(t, app, createDraftRequest(t, app, otherUser, todayMoscow(), 1))
	if err := app.handleBotContext(context.Background(), BotContext{User: adminCtx, Payload: fmt.Sprintf("admin:reject_reason:%d:Недостаточно данных", rejected.ID)}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if got := requestStatus(t, app, rejected.ID); got != "rejected" {
		t.Fatalf("expected rejected, got %s", got)
	}
	rejected = mustRequestByNumber(t, app, rejected.RequestNumber)
	if !rejected.PublicComment.Valid || rejected.PublicComment.String != "Недостаточно данных" {
		t.Fatalf("expected rejection reason, got %#v", rejected.PublicComment)
	}

	clarifyUser := createConsentedUser(t, app, 3003, "Уточнение")
	clarified := mustRequestByNumber(t, app, createDraftRequest(t, app, clarifyUser, todayMoscow(), 1))
	app.setSession(admin.MaxUserID, "admin_clarify", map[string]string{"request_id": fmt.Sprint(clarified.ID)})
	if err := app.handleBotContext(context.Background(), BotContext{User: adminCtx, Text: "Уточните цель визита"}); err != nil {
		t.Fatalf("clarify: %v", err)
	}
	if got := requestStatus(t, app, clarified.ID); got != "clarification_requested" {
		t.Fatalf("expected clarification status, got %s", got)
	}

	app.setSession(clarifyUser.MaxUserID, "initiator_clarify_answer", map[string]string{"request_id": fmt.Sprint(clarified.ID)})
	if err := app.handleBotContext(context.Background(), BotContext{User: testMaxUser(clarifyUser.MaxUserID, clarifyUser.DisplayName), Text: "Иду на консультацию"}); err != nil {
		t.Fatalf("answer clarification: %v", err)
	}
	if got := requestStatus(t, app, clarified.ID); got != "pending_review" {
		t.Fatalf("expected back to pending, got %s", got)
	}

	clarified = mustRequestByNumber(t, app, clarified.RequestNumber)
	card := app.requestCardText(clarified)
	if !strings.Contains(card, "Вопрос: Уточните цель визита") || !strings.Contains(card, "Ответ: Иду на консультацию") {
		t.Fatalf("expected clarification question and answer in request card, got %q", card)
	}
}

func TestEntryConfirmationFirstAndRepeat(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 4001, "Админ").MaxUserID, roleAdmin)
	req := mustRequestByNumber(t, app, createDraftRequest(t, app, initiator, todayMoscow(), 1))
	if err := app.updateRequestStatus(req.ID, "approved", admin, "approved", ""); err != nil {
		t.Fatalf("approve status: %v", err)
	}

	entryType, err := app.registerEntry(req.ID, admin)
	if err != nil {
		t.Fatalf("first entry: %v", err)
	}
	if entryType != "first_entry" || requestStatus(t, app, req.ID) != "passed" || app.entryCount(req.ID) != 1 {
		t.Fatalf("unexpected first entry result")
	}
	entryType, err = app.registerEntry(req.ID, admin)
	if err != nil {
		t.Fatalf("repeat entry: %v", err)
	}
	if entryType != "re_entry" || app.entryCount(req.ID) != 2 {
		t.Fatalf("unexpected repeat entry result")
	}
}

func TestEntryConfirmationRejectedForWrongStatusAndWrongDate(t *testing.T) {
	app := newTestApp(t)
	initiator := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 4101, "Админ").MaxUserID, roleAdmin)

	pending := mustRequestByNumber(t, app, createDraftRequest(t, app, initiator, todayMoscow(), 1))
	if _, err := app.registerEntry(pending.ID, admin); err == nil {
		t.Fatalf("pending request must not allow entry")
	}

	other := createConsentedUser(t, app, 4102, "Вчера")
	pastDate := addDays(todayMoscow(), -1)
	_, err := app.exec(`
		INSERT INTO pass_requests (request_number, user_id, full_name, visit_date, visit_time, zone_id, visit_purpose, status, created_at, updated_at)
		VALUES ('PASS-PAST-ENTRY', ?, 'Вчерашний Гость', ?, '09:00', 1, 'Тест', 'approved', ?, ?)
	`, other.ID, pastDate, nowISO(), nowISO())
	if err != nil {
		t.Fatalf("insert past approved: %v", err)
	}
	past := mustRequestByNumber(t, app, "PASS-PAST-ENTRY")
	if _, err := app.registerEntry(past.ID, admin); err == nil {
		t.Fatalf("past request must not allow entry")
	}
}

func TestExpireOldRequestsMarksNoShowAndExpired(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	pastDate := addDays(todayMoscow(), -1)
	now := nowISO()
	_, err := app.exec(`
		INSERT INTO pass_requests (request_number, user_id, full_name, visit_date, visit_time, zone_id, visit_purpose, status, created_at, updated_at)
		VALUES
			('PASS-OLD-APPROVED', ?, 'Старый Одобренный', ?, '09:00', 1, 'Тест', 'approved', ?, ?),
			('PASS-OLD-CLARIFY', ?, 'Старый Уточнение', ?, '09:00', 1, 'Тест', 'clarification_requested', ?, ?)
	`, user.ID, pastDate, now, now, user.ID, pastDate, now, now)
	if err != nil {
		t.Fatalf("insert old requests: %v", err)
	}
	if err := app.expireOldRequests(context.Background()); err != nil {
		t.Fatalf("expire old: %v", err)
	}
	if got := requestStatus(t, app, mustRequestByNumber(t, app, "PASS-OLD-APPROVED").ID); got != "no_show" {
		t.Fatalf("expected no_show, got %s", got)
	}
	if got := requestStatus(t, app, mustRequestByNumber(t, app, "PASS-OLD-CLARIFY").ID); got != "expired" {
		t.Fatalf("expected expired, got %s", got)
	}
}

func TestRequestHistoryEntriesListsAndExport(t *testing.T) {
	app := newTestApp(t)
	user := upsertConsentedUser(t, app)
	admin := setUserRole(t, app, createConsentedUser(t, app, 5001, "Админ").MaxUserID, roleAdmin)
	req := mustRequestByNumber(t, app, createDraftRequest(t, app, user, todayMoscow(), 1))
	if err := app.updateRequestStatus(req.ID, "approved", admin, "approved", ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := app.registerEntry(req.ID, admin); err != nil {
		t.Fatalf("entry: %v", err)
	}

	if err := app.showHistory(context.Background(), BotContext{User: testUser()}, req.ID); err != nil {
		t.Fatalf("history: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "approved") && !strings.Contains(lastReply(t, app).Text, "одоб") {
		t.Fatalf("expected history text, got %q", lastReply(t, app).Text)
	}
	if err := app.showEntries(context.Background(), BotContext{User: testUser()}, req.ID); err != nil {
		t.Fatalf("entries: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "проход") {
		t.Fatalf("expected entry text, got %q", lastReply(t, app).Text)
	}
	if err := app.showMyRequests(context.Background(), BotContext{User: testUser()}, user); err != nil {
		t.Fatalf("my requests: %v", err)
	}
	if err := app.showMyEntries(context.Background(), BotContext{User: testUser()}, user); err != nil {
		t.Fatalf("my entries: %v", err)
	}
	if err := app.exportActiveToday(context.Background(), BotContext{User: testMaxUser(admin.MaxUserID, admin.DisplayName)}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "Excel-экспорт сформирован") {
		t.Fatalf("expected xlsx export confirmation, got %q", lastReply(t, app).Text)
	}

	content, activeCount, closedCount, err := app.buildTodayRequestsWorkbook()
	if err != nil {
		t.Fatalf("build xlsx: %v", err)
	}
	if activeCount != 1 || closedCount != 0 {
		t.Fatalf("unexpected export counts: active=%d closed=%d", activeCount, closedCount)
	}
	book, err := excelize.OpenReader(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("open xlsx: %v", err)
	}
	defer func() { _ = book.Close() }()
	sheets := book.GetSheetList()
	if !contains(sheets, "Активные заявки") || !contains(sheets, "Закрытые заявки") {
		t.Fatalf("expected active and closed sheets, got %#v", sheets)
	}
	value, err := book.GetCellValue("Активные заявки", "A2")
	if err != nil {
		t.Fatalf("read active sheet: %v", err)
	}
	if value != req.RequestNumber {
		t.Fatalf("expected exported request number %q, got %q", req.RequestNumber, value)
	}
}

func TestTechAdminCanListGrantRevokeAndAuditAdmins(t *testing.T) {
	app := newTestApp(t)
	tech := setUserRole(t, app, createConsentedUser(t, app, 6001, "Тех Админ").MaxUserID, roleTechAdmin)
	target := createConsentedUser(t, app, 6002, "Будущий Админ")

	if err := app.showAdmins(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, roleAdmin); err != nil {
		t.Fatalf("show admins: %v", err)
	}
	if !hasButtonText(lastReply(t, app).Rows, "Добавить админа") {
		t.Fatalf("expected add admin button")
	}

	if err := app.grantAdminFromText(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, tech, fmt.Sprint(target.MaxUserID)); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	granted, err := app.userByMaxID(target.MaxUserID)
	if err != nil {
		t.Fatalf("target role: %v", err)
	}
	if granted == nil || granted.Role != roleAdmin {
		t.Fatalf("expected admin role, got %#v", granted)
	}
	if auditCount(t, app, tech.MaxUserID, "admin_role_granted") != 1 {
		t.Fatalf("expected grant audit")
	}

	if err := app.showAdminDetails(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, target.MaxUserID); err != nil {
		t.Fatalf("admin details: %v", err)
	}
	if !hasButtonText(lastReply(t, app).Rows, "Забрать роль админа") {
		t.Fatalf("expected revoke button")
	}
	if err := app.showAdminAudit(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, tech.MaxUserID); err != nil {
		t.Fatalf("admin audit: %v", err)
	}

	if err := app.revokeAdmin(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, tech, target.MaxUserID); err != nil {
		t.Fatalf("revoke admin: %v", err)
	}
	revoked, err := app.userByMaxID(target.MaxUserID)
	if err != nil {
		t.Fatalf("revoked role: %v", err)
	}
	if revoked == nil || revoked.Role != roleInitiator {
		t.Fatalf("expected initiator role after revoke, got %#v", revoked)
	}
}

func TestTechAdminCannotGrantUnknownUserOrRevokeTechAdmin(t *testing.T) {
	app := newTestApp(t)
	tech := setUserRole(t, app, createConsentedUser(t, app, 6101, "Тех Админ").MaxUserID, roleTechAdmin)
	otherTech := setUserRole(t, app, createConsentedUser(t, app, 6102, "Другой Тех").MaxUserID, roleTechAdmin)

	if err := app.grantAdminFromText(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, tech, "999999"); err != nil {
		t.Fatalf("grant unknown should be handled as reply, got err: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "не найден") {
		t.Fatalf("expected not found reply, got %q", lastReply(t, app).Text)
	}

	if err := app.revokeAdmin(context.Background(), BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName)}, tech, otherTech.MaxUserID); err != nil {
		t.Fatalf("revoke tech should be handled as reply, got err: %v", err)
	}
	if !strings.Contains(lastReply(t, app).Text, "технического администратора") {
		t.Fatalf("expected tech admin protection reply, got %q", lastReply(t, app).Text)
	}
}

func TestTechAdminCanManageZones(t *testing.T) {
	app := newTestApp(t)
	tech := setUserRole(t, app, createConsentedUser(t, app, 6201, "Тех Админ").MaxUserID, roleTechAdmin)
	ctx := context.Background()

	if err := app.handleBotContext(ctx, BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName), Payload: "tech:add_zone"}); err != nil {
		t.Fatalf("add zone prompt: %v", err)
	}
	if err := app.handleBotContext(ctx, BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName), Text: "Новый корпус"}); err != nil {
		t.Fatalf("zone name: %v", err)
	}
	if err := app.handleBotContext(ctx, BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName), Text: "г. Москва, тестовый адрес"}); err != nil {
		t.Fatalf("zone address: %v", err)
	}
	var zoneID int64
	if err := app.queryRow(`SELECT id FROM zones WHERE short_name = 'Новый корпус'`).Scan(&zoneID); err != nil {
		t.Fatalf("new zone not found: %v", err)
	}
	if err := app.handleBotContext(ctx, BotContext{User: testMaxUser(tech.MaxUserID, tech.DisplayName), Payload: fmt.Sprintf("tech:toggle_zone:%d", zoneID)}); err != nil {
		t.Fatalf("toggle zone: %v", err)
	}
	var active int
	if err := app.queryRow(`SELECT is_active FROM zones WHERE id = ?`, zoneID).Scan(&active); err != nil {
		t.Fatalf("zone active: %v", err)
	}
	if active != 0 {
		t.Fatalf("expected inactive zone after toggle, got %d", active)
	}
}

func TestTechAdminExtraFieldsAreCollectedAndShownInRequest(t *testing.T) {
	app := newTestApp(t)
	tech := setUserRole(t, app, createConsentedUser(t, app, 6301, "Тех Админ").MaxUserID, roleTechAdmin)
	user := upsertConsentedUser(t, app)

	NewScenario(t, app, testMaxUser(tech.MaxUserID, tech.DisplayName)).
		ClickPayload("tech:add_extra_fields").
		ExpectText("Напишите названия дополнительных полей").
		Say("Номер машины\nКомментарий для проходной").
		ExpectText("Дополнительные поля формы").
		ExpectButton("Вкл: Номер машины").
		ExpectButton("Вкл: Комментарий для проходной")

	if err := app.ensureDraft(user.ID); err != nil {
		t.Fatalf("ensure draft: %v", err)
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{
		"full_name":     "Иванов Иван Иванович",
		"visit_date":    addDays(todayMoscow(), 1),
		"visit_time":    "09:00",
		"zone_id":       int64(1),
		"visit_purpose": "Встреча на кафедре",
	}); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if _, err := app.createPassRequest(user); err == nil {
		t.Fatalf("request with missing active extra fields must be rejected")
	}
	fields, err := app.activeExtraFields()
	if err != nil {
		t.Fatalf("active fields: %v", err)
	}
	for _, field := range fields {
		value := "А123ВС77"
		if field.Label == "Комментарий для проходной" {
			value = "Пропустить к главному входу"
		}
		if err := app.setDraftExtraField(user.ID, field.ID, field.Label, value); err != nil {
			t.Fatalf("set extra field: %v", err)
		}
	}
	number, err := app.createPassRequest(user)
	if err != nil {
		t.Fatalf("create request with extra fields: %v", err)
	}
	req := mustRequestByNumber(t, app, number)
	card := app.requestCardText(req)
	if !strings.Contains(card, "Номер машины: А123ВС77") || !strings.Contains(card, "Комментарий для проходной: Пропустить к главному входу") {
		t.Fatalf("expected extra fields in request card, got %q", card)
	}
}

func TestAdminQueueOrdersByVisitDateAndTime(t *testing.T) {
	app := newTestApp(t)
	admin := setUserRole(t, app, createConsentedUser(t, app, 6401, "Админ").MaxUserID, roleAdmin)
	user := upsertConsentedUser(t, app)
	now := nowISO()
	rows := []struct {
		number string
		date   string
		time   string
	}{
		{"PASS-SORT-3", addDays(todayMoscow(), 2), "09:00"},
		{"PASS-SORT-1", addDays(todayMoscow(), 1), "15:00"},
		{"PASS-SORT-2", addDays(todayMoscow(), 1), "09:00"},
	}
	for _, item := range rows {
		if _, err := app.exec(`
			INSERT INTO pass_requests (request_number, user_id, full_name, visit_date, visit_time, zone_id, visit_purpose, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 1, 'Тест сортировки', 'pending_review', ?, ?)
		`, item.number, user.ID, item.number, item.date, item.time, now, now); err != nil {
			t.Fatalf("insert sorted request: %v", err)
		}
	}
	if err := app.adminQueue(context.Background(), BotContext{User: testMaxUser(admin.MaxUserID, admin.DisplayName)}, "pending_review", 0); err != nil {
		t.Fatalf("admin queue: %v", err)
	}
	text := lastReply(t, app).Text
	first := strings.Index(text, "PASS-SORT-2")
	second := strings.Index(text, "PASS-SORT-1")
	third := strings.Index(text, "PASS-SORT-3")
	if first < 0 || second < 0 || third < 0 || !(first < second && second < third) {
		t.Fatalf("expected queue ordered by visit date/time, got %q", text)
	}
}

func TestGroupUpdatesByUserKeepsPerUserOrder(t *testing.T) {
	updates := []Update{
		{UpdateType: "message_callback", Callback: &Callback{User: testMaxUser(1, "one"), Payload: "draft:start"}},
		{UpdateType: "message_callback", Callback: &Callback{User: testMaxUser(2, "two"), Payload: "draft:start"}},
		{UpdateType: "message_created", Message: &Message{Sender: &MaxUser{UserID: 1, Name: "one"}}},
	}
	groups := groupUpdatesByUser(updates)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if updateUserID(groups[0][0]) != 1 || updateUserID(groups[0][1]) != 1 {
		t.Fatalf("expected first user updates grouped together")
	}
}

func todayMoscowAsHuman() string {
	t, err := time.Parse("2006-01-02", todayMoscow())
	if err != nil {
		return fmt.Sprintf("24.05.2026")
	}
	return t.Format("02.01.2006")
}

func safeVisitTime(visitDate string) string {
	if visitDate == todayMoscow() {
		return "23:59"
	}
	return "09:00"
}
