package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// poll забирает события MAX и передает их в обработку.
func (app *App) poll(ctx context.Context) error {
	var marker int64
	for {
		pollStarted := time.Now()
		updates, err := app.api.GetUpdates(ctx, marker)
		pollDuration := time.Since(pollStarted)
		if err != nil {
			if app.metrics != nil {
				app.metrics.pollErrorsTotal.Add(1)
			}
			log.Printf("get updates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		app.observePoll(updates.Updates, pollDuration)
		marker = updates.Marker
		var wg sync.WaitGroup
		sem := make(chan struct{}, 8)
		// MAX иногда отдаёт накопившиеся события пачкой. Разные пользователи
		// обрабатываются параллельно, но внутри одного пользователя порядок
		// сохраняется: иначе быстрый ввод ФИО мог бы обогнать нажатие "Создать".
		for _, group := range groupUpdatesByUser(updates.Updates) {
			group := group
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				updateCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
				defer cancel()
				for _, update := range group {
					if app.metrics != nil {
						app.metrics.incUpdate(update.UpdateType)
					}
					if err := app.handleUpdate(updateCtx, update); err != nil {
						if app.metrics != nil {
							app.metrics.updateErrors.Add(1)
						}
						log.Printf("handle update: %v", err)
					}
				}
			}()
		}
		wg.Wait()
	}
}

// observePoll сохраняет размер пачки, длительность polling и задержку событий.
func (app *App) observePoll(updates []Update, pollDuration time.Duration) {
	if app.metrics != nil {
		app.metrics.lastPollDurationMillis.Store(uint64(pollDuration.Milliseconds()))
		app.metrics.lastBatchSize.Store(uint64(len(updates)))
	}
	if len(updates) == 0 {
		return
	}

	var maxLag time.Duration
	for _, update := range updates {
		lag, ok := updateLag(update)
		if !ok {
			continue
		}
		if app.metrics != nil {
			app.metrics.observeUpdateLag(lag)
		}
		if lag > maxLag {
			maxLag = lag
		}
	}
	if len(updates) > 3 || maxLag > 3*time.Second {
		// Эта строка нужна для разборов "бот завис": она отделяет задержку
		// доставки событий MAX от времени, которое реально потратил наш код.
		log.Printf("max updates batch: count=%d poll_duration=%s max_update_lag=%s", len(updates), pollDuration.Round(time.Millisecond), maxLag.Round(time.Millisecond))
	}
}

// updateLag считает задержку между событием MAX и обработкой у нас.
func updateLag(update Update) (time.Duration, bool) {
	timestamp := update.Timestamp
	if timestamp == 0 && update.Callback != nil {
		timestamp = update.Callback.Timestamp
	}
	if timestamp <= 0 {
		return 0, false
	}

	var updateTime time.Time
	switch {
	case timestamp > 1_000_000_000_000:
		updateTime = time.UnixMilli(timestamp)
	case timestamp > 1_000_000_000:
		updateTime = time.Unix(timestamp, 0)
	default:
		return 0, false
	}
	lag := time.Since(updateTime)
	if lag < 0 {
		lag = 0
	}
	return lag, true
}

// groupUpdatesByUser группирует события по пользователям, сохраняя порядок внутри одного чата.
func groupUpdatesByUser(updates []Update) [][]Update {
	groups := make([][]Update, 0)
	index := map[int64]int{}
	for _, update := range updates {
		userID := updateUserID(update)
		if userID == 0 {
			groups = append(groups, []Update{update})
			continue
		}
		pos, ok := index[userID]
		if !ok {
			index[userID] = len(groups)
			groups = append(groups, []Update{})
			pos = len(groups) - 1
		}
		groups[pos] = append(groups[pos], update)
	}
	return groups
}

// updateUserID достает пользователя из события MAX независимо от его типа.
func updateUserID(update Update) int64 {
	switch update.UpdateType {
	case "message_created":
		if update.Message != nil && update.Message.Sender != nil {
			return update.Message.Sender.UserID
		}
	case "message_callback":
		if update.Callback != nil {
			return update.Callback.User.UserID
		}
	case "bot_started":
		if update.User != nil {
			return update.User.UserID
		}
	}
	return 0
}

// handleUpdate приводит сырое событие MAX к внутреннему контексту бота.
func (app *App) handleUpdate(ctx context.Context, update Update) error {
	switch update.UpdateType {
	case "bot_started":
		if update.User == nil {
			return nil
		}
		return app.handleBotContext(ctx, BotContext{User: *update.User, Text: "/start"})
	case "message_created":
		if update.Message == nil || update.Message.Sender == nil || update.Message.Sender.IsBot {
			return nil
		}
		return app.handleBotContext(ctx, BotContext{
			User: *update.Message.Sender,
			Text: strings.TrimSpace(update.Message.Body.Text),
		})
	case "message_callback":
		if update.Callback == nil {
			return nil
		}
		return app.handleBotContext(ctx, BotContext{
			User:       update.Callback.User,
			Payload:    update.Callback.Payload,
			CallbackID: update.Callback.CallbackID,
		})
	}
	return nil
}

// skipDuplicateStart отсекает повторный /start, если MAX прислал его дважды подряд.
func (app *App) skipDuplicateStart(maxUserID int64) bool {
	app.startMu.Lock()
	defer app.startMu.Unlock()
	if app.lastStart == nil {
		app.lastStart = map[int64]time.Time{}
	}
	now := time.Now()
	if last, ok := app.lastStart[maxUserID]; ok && now.Sub(last) < 2*time.Second {
		return true
	}
	app.lastStart[maxUserID] = now
	return false
}

// handleBotContext ведет общий сценарий обработки текста и кнопок.
func (app *App) handleBotContext(ctx context.Context, bctx BotContext) error {
	user, err := app.upsertUser(bctx.User)
	if err != nil {
		return err
	}

	if bctx.Text != "" && strings.HasPrefix(bctx.Text, "/") {
		return app.handleCommand(ctx, bctx, user)
	}

	if isMainMenuText(bctx.Text) {
		app.clearSession(user.MaxUserID)
		if !app.hasConsent(user.ID) {
			return app.showConsent(ctx, bctx, user)
		}
		return app.reply(ctx, bctx, "Главное меню", app.mainMenu(user))
	}

	if bctx.Payload != "" {
		return app.handleCallback(ctx, bctx, user)
	}

	if !app.hasConsent(user.ID) {
		return app.showConsent(ctx, bctx, user)
	}

	session, ok := app.getSession(user.MaxUserID)
	if ok {
		return app.handleSessionText(ctx, bctx, user, session)
	}

	if handled, err := app.tryRecoverDraftInput(ctx, bctx, user); handled || err != nil {
		return err
	}

	if app.isTechAdmin(user) && looksLikeUserID(bctx.Text) {
		return app.grantAdminFromText(ctx, bctx, user, bctx.Text)
	}
	if app.isAdmin(user) && looksLikeRequestNumberQuery(bctx.Text) {
		req, err := app.requestByNumber(bctx.Text)
		if err != nil {
			return err
		}
		if req == nil {
			return app.reply(ctx, bctx, "Заявка не найдена.", mainMenuRows())
		}
		return app.showRequestCard(ctx, bctx, user, *req)
	}

	return app.reply(ctx, bctx, "Откройте меню командой /start.", app.mainMenu(user))
}

// reply отправляет ответ пользователю или сохраняет его в тестовом режиме.
func (app *App) reply(ctx context.Context, bctx BotContext, text string, rows [][]Button) error {
	if app.metrics != nil {
		app.metrics.repliesTotal.Add(1)
	}
	if app.api == nil {
		app.testReplies = append(app.testReplies, TestReply{Text: text, Rows: rows})
		return nil
	}
	if bctx.CallbackID != "" {
		return app.api.AnswerCallback(ctx, bctx.CallbackID, text, rows)
	}
	return app.api.SendToUser(ctx, bctx.User.UserID, text, rows)
}

// handleCommand обрабатывает /start и служебный bootstrap-код.
func (app *App) handleCommand(ctx context.Context, bctx BotContext, user UserRow) error {
	fields := strings.Fields(bctx.Text)
	command := fields[0]
	switch command {
	case "/start":
		if app.skipDuplicateStart(user.MaxUserID) {
			return nil
		}
		app.clearSession(user.MaxUserID)
		if !app.hasConsent(user.ID) {
			return app.showConsent(ctx, bctx, user)
		}
		return app.reply(ctx, bctx, "Главное меню", app.mainMenu(user))
	case "/whoami":
		return app.reply(ctx, bctx, fmt.Sprintf("MAX user id: %d\nРоль: %s", user.MaxUserID, roleLabel(user.Role)), app.mainMenu(user))
	case "/reset_session":
		app.clearSession(user.MaxUserID)
		return app.reply(ctx, bctx, "Текущий текстовый ввод сброшен.", app.mainMenu(user))
	case "/admin":
		if app.cfg.AdminBootstrapCode == "" || len(fields) < 2 || fields[1] != app.cfg.AdminBootstrapCode {
			return app.reply(ctx, bctx, "Код администратора не принят.", mainMenuRows())
		}
		if err := app.setRole(user.MaxUserID, roleAdmin); err != nil {
			return err
		}
		user.Role = roleAdmin
		return app.reply(ctx, bctx, "Роль администратора выдана.", app.mainMenu(user))
	}
	return app.reply(ctx, bctx, "Неизвестная команда. Используйте /start.", mainMenuRows())
}

// showConsent показывает стартовый экран с согласием на обработку данных.
func (app *App) showConsent(ctx context.Context, bctx BotContext, user UserRow) error {
	text := strings.Join([]string{
		"Весенний_код_1",
		"",
		"Прототип сервиса оформления разового гостевого пропуска, разработанный командой хакатона. Сервис не является официальной функцией платформы MAX.",
		"",
		"Для оформления и сопровождения заявки мы сохраняем: MAX user id, отображаемое имя, ФИО, дату и примерное время визита, корпус/зону, цель визита, статус заявки и историю действий.",
		"",
		"Мы не запрашиваем паспортные данные, банковские данные и номер телефона.",
		"",
		"Нажимая «Согласен», вы подтверждаете согласие на обработку указанных данных для оформления гостевого пропуска и уведомлений по заявке.",
		"",
		"Версия согласия: " + app.cfg.PolicyVersion,
	}, "\n")
	return app.reply(ctx, bctx, text, [][]Button{
		{btn("Согласен", "consent:accept", "positive")},
		{btn("Политика данных", "data:policy", "")},
	})
}

// mainMenu собирает кнопки главного меню с учетом роли пользователя.
func (app *App) mainMenu(user UserRow) [][]Button {
	rows := [][]Button{
		{btn("Создать пропуск", "draft:start", "positive")},
		{btn("Мои заявки", "my:list", ""), btn("Мои проходы", "my:entries", "")},
		{btn("Политика и согласие", "data:policy", "")},
	}
	if app.isAdmin(user) {
		rows = append(rows, []Button{btn("Меню админа", "admin:menu", "")})
	}
	if app.isTechAdmin(user) {
		rows = append(rows, []Button{btn("Меню тех админа", "tech:menu", "")})
	}
	return rows
}

// btn создает inline-кнопку MAX в короткой записи.
func btn(text, payload, intent string) Button {
	return Button{Type: "callback", Text: text, Payload: payload, Intent: intent}
}

// mainMenuRows возвращает стандартную кнопку выхода в главное меню.
func mainMenuRows() [][]Button {
	return [][]Button{{btn("Главное меню", "menu", "")}}
}

// adminBackRows возвращает кнопки возврата для админских экранов.
func adminBackRows() [][]Button {
	return [][]Button{{btn("Меню админа", "admin:menu", ""), btn("Главное меню", "menu", "")}}
}

// techBackRows возвращает кнопки возврата для техадминских экранов.
func techBackRows() [][]Button {
	return [][]Button{{btn("Меню тех админа", "tech:menu", ""), btn("Главное меню", "menu", "")}}
}

// draftBackRows возвращает кнопки назад и в меню во время заполнения анкеты.
func draftBackRows(backPayload string) [][]Button {
	return [][]Button{{btn("Назад", backPayload, ""), btn("Главное меню", "menu", "")}}
}

// dateInputBackPayload выбирает правильный возврат для ручного ввода даты.
func dateInputBackPayload(session Session) string {
	if session.Data["mode"] == "edit" {
		return "draft:edit"
	}
	return "draft:back_full_name"
}

// timeInputBackPayload выбирает правильный возврат для ручного ввода времени.
func timeInputBackPayload(session Session) string {
	if session.Data["mode"] == "edit" {
		return "draft:edit"
	}
	return "draft:back_date"
}

// parsePayload разбирает callback payload на раздел, действие и id.
func parsePayload(payload string) (scope, action, id, extra string) {
	scope, rest, ok := strings.Cut(payload, ":")
	if !ok {
		return scope, "", "", ""
	}
	action, rest, ok = strings.Cut(rest, ":")
	if !ok {
		return scope, action, "", ""
	}
	if scope == "admin" && action == "reject_reason" {
		id, extra, _ = strings.Cut(rest, ":")
		return scope, action, id, extra
	}
	return scope, action, rest, ""
}

// handleCallback отправляет нажатие кнопки в нужный обработчик.
func (app *App) handleCallback(ctx context.Context, bctx BotContext, user UserRow) error {
	if bctx.Payload == "consent:accept" {
		_, err := app.exec(`
			INSERT INTO consents (user_id, document_version, scope, accepted_at)
			VALUES (?, ?, 'profile_and_pass_requests', ?)
			ON CONFLICT(user_id, document_version, scope) DO NOTHING
		`, user.ID, app.cfg.PolicyVersion, nowISO())
		if err != nil {
			return err
		}
		app.audit(user.MaxUserID, "consent_accepted", "user", user.ID, map[string]string{"policy": app.cfg.PolicyVersion})
		return app.reply(ctx, bctx, "Согласие сохранено.", app.mainMenu(user))
	}
	if bctx.Payload == "data:policy" {
		return app.showDataPolicy(ctx, bctx, user)
	}

	if !app.hasConsent(user.ID) {
		return app.showConsent(ctx, bctx, user)
	}

	scope, action, id, extra := parsePayload(bctx.Payload)

	switch scope {
	case "menu":
		app.clearSession(user.MaxUserID)
		return app.reply(ctx, bctx, "Главное меню", app.mainMenu(user))
	case "draft":
		return app.handleDraftCallback(ctx, bctx, user, action, id)
	case "my":
		if action == "list" {
			return app.showMyRequests(ctx, bctx, user)
		}
		if action == "entries" {
			return app.showMyEntries(ctx, bctx, user)
		}
	case "request":
		return app.handleRequestCallback(ctx, bctx, user, action, id)
	case "data":
		return app.handleDataCallback(ctx, bctx, user, action)
	case "admin":
		return app.handleAdminCallback(ctx, bctx, user, action, id, extra)
	case "tech":
		return app.handleTechCallback(ctx, bctx, user, action, id)
	}

	return app.reply(ctx, bctx, "Неизвестное действие. Откройте меню командой /start.", mainMenuRows())
}

// handleDraftCallback ведет кнопочный сценарий создания и правки анкеты.
func (app *App) handleDraftCallback(ctx context.Context, bctx BotContext, user UserRow, action, id string) error {
	switch action {
	case "start":
		draft, _ := app.getDraft(user.ID)
		if draft != nil {
			return app.reply(ctx, bctx, "У вас есть сохранённый черновик.", [][]Button{
				{btn("Продолжить", "draft:continue", "positive")},
				{btn("Начать заново", "draft:new", ""), btn("Удалить", "draft:delete", "negative")},
				{btn("Главное меню", "menu", "")},
			})
		}
		if err := app.ensureDraft(user.ID); err != nil {
			return err
		}
		app.setSession(user.MaxUserID, "draft_full_name", nil)
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("menu"))
	case "continue":
		return app.continueDraft(ctx, bctx, user)
	case "new":
		app.deleteDraft(user.ID)
		if err := app.ensureDraft(user.ID); err != nil {
			return err
		}
		app.setSession(user.MaxUserID, "draft_full_name", nil)
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("menu"))
	case "delete":
		app.deleteDraft(user.ID)
		return app.reply(ctx, bctx, "Черновик удалён.", app.mainMenu(user))
	case "save":
		return app.reply(ctx, bctx, "Черновик сохранён. Можно продолжить позже.", app.mainMenu(user))
	case "edit":
		return app.reply(ctx, bctx, "Что изменить?", [][]Button{
			{btn("ФИО", "draft:edit_full_name", ""), btn("Дата", "draft:edit_date", "")},
			{btn("Время", "draft:edit_time", ""), btn("Корпус/зона", "draft:edit_zone", "")},
			{btn("Цель", "draft:edit_purpose", ""), btn("Доп. поля", "draft:edit_extra_fields", "")},
			{btn("Назад", "draft:summary", ""), btn("Главное меню", "menu", "")},
		})
	case "summary":
		return app.showDraftSummary(ctx, bctx, user)
	case "back_full_name":
		app.setSession(user.MaxUserID, "draft_full_name", nil)
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("menu"))
	case "back_date":
		return app.askDate(ctx, bctx, user)
	case "back_time":
		return app.askTime(ctx, bctx)
	case "back_zone":
		return app.askZone(ctx, bctx)
	case "back_purpose":
		app.setSession(user.MaxUserID, "draft_purpose", nil)
		return app.reply(ctx, bctx, "Кратко опишите цель визита.", draftBackRows("draft:back_zone"))
	case "edit_full_name":
		app.setSession(user.MaxUserID, "draft_full_name", map[string]string{"mode": "edit"})
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("draft:edit"))
	case "edit_date":
		return app.askDateForEdit(ctx, bctx)
	case "edit_time":
		return app.askTimeForEdit(ctx, bctx)
	case "edit_zone":
		return app.askZoneForEdit(ctx, bctx)
	case "edit_purpose":
		app.setSession(user.MaxUserID, "draft_purpose", map[string]string{"mode": "edit"})
		return app.reply(ctx, bctx, "Кратко опишите цель визита.", draftBackRows("draft:edit"))
	case "edit_extra_fields":
		if err := app.updateDraft(user.ID, map[string]interface{}{"extra_fields_json": nil}); err != nil {
			return err
		}
		return app.askNextExtraFieldOrSummary(ctx, bctx, user)
	case "date":
		return app.setDraftDate(ctx, bctx, user, id)
	case "set_date_summary":
		if err := validateDate(id); err != nil {
			return app.reply(ctx, bctx, err.Error(), draftBackRows("draft:edit"))
		}
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_date": id}); err != nil {
			return err
		}
		return app.showDraftSummary(ctx, bctx, user)
	case "date_custom":
		app.setSession(user.MaxUserID, "draft_date_custom", nil)
		return app.reply(ctx, bctx, "Введите дату в формате ДД.ММ.ГГГГ.", draftBackRows("draft:back_full_name"))
	case "date_custom_summary":
		app.setSession(user.MaxUserID, "draft_date_custom", map[string]string{"mode": "edit"})
		return app.reply(ctx, bctx, "Введите дату в формате ДД.ММ.ГГГГ.", draftBackRows("draft:edit"))
	case "time":
		if errText := app.validateDraftVisitTime(user.ID, id); errText != "" {
			return app.reply(ctx, bctx, errText, draftBackRows("draft:back_date"))
		}
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_time": id}); err != nil {
			return err
		}
		return app.askZone(ctx, bctx)
	case "set_time_summary":
		if errText := app.validateDraftVisitTime(user.ID, id); errText != "" {
			return app.reply(ctx, bctx, errText, draftBackRows("draft:edit"))
		}
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_time": id}); err != nil {
			return err
		}
		return app.showDraftSummary(ctx, bctx, user)
	case "time_custom":
		app.setSession(user.MaxUserID, "draft_time_custom", nil)
		return app.reply(ctx, bctx, "Введите время в формате ЧЧ:ММ.", draftBackRows("draft:back_date"))
	case "time_custom_summary":
		app.setSession(user.MaxUserID, "draft_time_custom", map[string]string{"mode": "edit"})
		return app.reply(ctx, bctx, "Введите время в формате ЧЧ:ММ.", draftBackRows("draft:edit"))
	case "zone":
		zoneID, _ := strconv.ParseInt(id, 10, 64)
		if err := app.updateDraft(user.ID, map[string]interface{}{"zone_id": zoneID, "custom_zone_text": nil}); err != nil {
			return err
		}
		app.setSession(user.MaxUserID, "draft_purpose", nil)
		return app.reply(ctx, bctx, "Кратко опишите цель визита.", draftBackRows("draft:back_zone"))
	case "set_zone_summary":
		zoneID, _ := strconv.ParseInt(id, 10, 64)
		if err := app.updateDraft(user.ID, map[string]interface{}{"zone_id": zoneID, "custom_zone_text": nil}); err != nil {
			return err
		}
		return app.showDraftSummary(ctx, bctx, user)
	case "zone_custom":
		if err := app.updateDraft(user.ID, map[string]interface{}{"zone_id": nil, "custom_zone_text": "Другое"}); err != nil {
			return err
		}
		app.setSession(user.MaxUserID, "draft_purpose", nil)
		return app.reply(ctx, bctx, "Кратко опишите цель визита и уточните место.", draftBackRows("draft:back_zone"))
	case "zone_custom_summary":
		if err := app.updateDraft(user.ID, map[string]interface{}{"zone_id": nil, "custom_zone_text": "Другое"}); err != nil {
			return err
		}
		return app.showDraftSummary(ctx, bctx, user)
	case "submit":
		number, err := app.createPassRequest(user)
		if err != nil {
			return app.reply(ctx, bctx, err.Error(), draftBackRows("draft:summary"))
		}
		return app.reply(ctx, bctx, "Заявка отправлена на рассмотрение.\nНомер: "+number, [][]Button{
			{btn("Мои заявки", "my:list", ""), btn("Главное меню", "menu", "")},
		})
	}
	return app.reply(ctx, bctx, "Неизвестное действие черновика.", mainMenuRows())
}

// showDataPolicy показывает, какие данные бот хранит и зачем.
func (app *App) showDataPolicy(ctx context.Context, bctx BotContext, user UserRow) error {
	text := strings.Join([]string{
		"Политика данных",
		"",
		"Оператор прототипа: команда хакатона Весенний_код_1. Перед промышленной эксплуатацией оператор и юридические документы должны быть утверждены университетом.",
		"",
		"Цель обработки: оформление разового гостевого пропуска, сопровождение заявки, уведомления по заявке, аудит действий и диагностика работы сервиса.",
		"",
		"Состав данных: MAX user id, отображаемое имя, ФИО, дата и примерное время визита, корпус/зона, цель визита, дополнительные поля формы, статус заявки, история действий и проходов.",
		"",
		"Не собираются: паспортные данные, банковские данные, адрес проживания и номер телефона.",
		"",
		"Срок хранения для прототипа: до окончания демонстрации/хакатона и последующей технической очистки. В релизной версии срок должен быть закреплён внутренним регламентом.",
		"",
		"Вы можете отозвать согласие и запросить удаление данных. После отзыва бот удалит черновик, снимет согласие и обезличит данные ваших заявок, сохранив минимальный технический аудит.",
		"",
		"Версия документов: " + app.cfg.PolicyVersion,
	}, "\n")
	rows := [][]Button{{btn("Главное меню", "menu", "")}}
	if app.hasConsent(user.ID) {
		rows = [][]Button{
			{btn("Отозвать согласие", "data:withdraw_confirm", "negative")},
			{btn("Главное меню", "menu", "")},
		}
	} else {
		rows = [][]Button{
			{btn("Согласен", "consent:accept", "positive")},
			{btn("Назад", "menu", "")},
		}
	}
	return app.reply(ctx, bctx, text, rows)
}

// handleDataCallback обрабатывает согласие, отзыв и просмотр правил.
func (app *App) handleDataCallback(ctx context.Context, bctx BotContext, user UserRow, action string) error {
	switch action {
	case "policy":
		return app.showDataPolicy(ctx, bctx, user)
	case "withdraw_confirm":
		return app.reply(ctx, bctx, "Отозвать согласие и запросить удаление данных? Черновик будет удалён, заявки будут обезличены, работа с ботом остановится до нового согласия.", [][]Button{
			{btn("Да, отозвать", "data:withdraw", "negative")},
			{btn("Назад", "data:policy", ""), btn("Главное меню", "menu", "")},
		})
	case "withdraw":
		if err := app.withdrawConsent(ctx, user); err != nil {
			return err
		}
		return app.reply(ctx, bctx, "Согласие отозвано. Черновик удалён, данные заявок обезличены. Чтобы снова пользоваться ботом, откройте /start и примите согласие заново.", [][]Button{
			{btn("Открыть старт", "menu", "")},
		})
	}
	return app.reply(ctx, bctx, "Неизвестное действие с данными.", mainMenuRows())
}

// withdrawConsent отзывает согласие и обезличивает персональные поля заявок.
func (app *App) withdrawConsent(ctx context.Context, user UserRow) error {
	_ = ctx
	app.deleteDraft(user.ID)
	app.clearSession(user.MaxUserID)
	if _, err := app.exec(`
		UPDATE pass_requests
		SET full_name = 'Удалено по запросу пользователя',
			visit_purpose = 'Удалено по запросу пользователя',
			custom_zone_text = NULL,
			extra_fields_json = NULL,
			public_comment = NULL,
			status = CASE
				WHEN status IN ('pending_review', 'clarification_requested', 'approved') THEN 'data_erasure_requested'
				ELSE status
			END,
			updated_at = ?
		WHERE user_id = ?
	`, nowISO(), user.ID); err != nil {
		return err
	}
	if _, err := app.exec(`DELETE FROM consents WHERE user_id = ?`, user.ID); err != nil {
		return err
	}
	app.audit(user.MaxUserID, "consent_withdrawn", "user", user.ID, map[string]string{"policy": app.cfg.PolicyVersion})
	return nil
}

// continueDraft продолжает черновик с того места, где пользователь остановился.
func (app *App) continueDraft(ctx context.Context, bctx BotContext, user UserRow) error {
	draft, err := app.getDraft(user.ID)
	if err != nil {
		return err
	}
	if draft == nil {
		if err := app.ensureDraft(user.ID); err != nil {
			return err
		}
		app.setSession(user.MaxUserID, "draft_full_name", nil)
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("menu"))
	}
	if !draft.FullName.Valid {
		app.setSession(user.MaxUserID, "draft_full_name", nil)
		return app.reply(ctx, bctx, "Введите ФИО полностью.", draftBackRows("menu"))
	}
	if !draft.VisitDate.Valid {
		return app.askDate(ctx, bctx, user)
	}
	if !draft.VisitTime.Valid {
		return app.askTime(ctx, bctx)
	}
	if !draft.ZoneID.Valid && !draft.CustomZoneText.Valid {
		return app.askZone(ctx, bctx)
	}
	if !draft.VisitPurpose.Valid {
		app.setSession(user.MaxUserID, "draft_purpose", nil)
		return app.reply(ctx, bctx, "Кратко опишите цель визита.", draftBackRows("draft:back_zone"))
	}
	return app.askNextExtraFieldOrSummary(ctx, bctx, user)
}

// askDate предлагает быстрый выбор даты посещения.
func (app *App) askDate(ctx context.Context, bctx BotContext, user UserRow) error {
	today := todayMoscow()
	return app.reply(ctx, bctx, "Выберите дату посещения.", [][]Button{
		{btn("Сегодня", "draft:date:"+today, ""), btn("Завтра", "draft:date:"+addDays(today, 1), "")},
		{btn("Послезавтра", "draft:date:"+addDays(today, 2), ""), btn("Другая дата", "draft:date_custom", "")},
		{btn("Назад", "draft:back_full_name", ""), btn("Главное меню", "menu", "")},
	})
}

// askDateForEdit просит новую дату при редактировании анкеты.
func (app *App) askDateForEdit(ctx context.Context, bctx BotContext) error {
	today := todayMoscow()
	return app.reply(ctx, bctx, "Выберите новую дату посещения.", [][]Button{
		{btn("Сегодня", "draft:set_date_summary:"+today, ""), btn("Завтра", "draft:set_date_summary:"+addDays(today, 1), "")},
		{btn("Послезавтра", "draft:set_date_summary:"+addDays(today, 2), ""), btn("Другая дата", "draft:date_custom_summary", "")},
		{btn("Назад", "draft:edit", ""), btn("Главное меню", "menu", "")},
	})
}

// setDraftDate сохраняет дату и перепроверяет время, если оно уже выбрано.
func (app *App) setDraftDate(ctx context.Context, bctx BotContext, user UserRow, date string) error {
	if err := validateDate(date); err != nil {
		return app.reply(ctx, bctx, err.Error(), draftBackRows("draft:back_full_name"))
	}
	if err := app.updateDraft(user.ID, map[string]interface{}{"visit_date": date}); err != nil {
		return err
	}
	if app.isEditSession(user.MaxUserID, "draft_date_select") {
		app.clearSession(user.MaxUserID)
		return app.showDraftSummary(ctx, bctx, user)
	}
	return app.askTime(ctx, bctx)
}

// askTime предлагает кнопки с доступным временем посещения.
func (app *App) askTime(ctx context.Context, bctx BotContext) error {
	return app.reply(ctx, bctx, "Во сколько планируете прийти?", [][]Button{
		{btn("09:00", "draft:time:09:00", ""), btn("11:00", "draft:time:11:00", ""), btn("13:00", "draft:time:13:00", "")},
		{btn("15:00", "draft:time:15:00", ""), btn("17:00", "draft:time:17:00", ""), btn("Другое время", "draft:time_custom", "")},
		{btn("Назад", "draft:back_date", ""), btn("Главное меню", "menu", "")},
	})
}

// askTimeForEdit просит новое время при редактировании анкеты.
func (app *App) askTimeForEdit(ctx context.Context, bctx BotContext) error {
	return app.reply(ctx, bctx, "Во сколько планируете прийти?", [][]Button{
		{btn("09:00", "draft:set_time_summary:09:00", ""), btn("11:00", "draft:set_time_summary:11:00", ""), btn("13:00", "draft:set_time_summary:13:00", "")},
		{btn("15:00", "draft:set_time_summary:15:00", ""), btn("17:00", "draft:set_time_summary:17:00", ""), btn("Другое время", "draft:time_custom_summary", "")},
		{btn("Назад", "draft:edit", ""), btn("Главное меню", "menu", "")},
	})
}

// askZone предлагает выбрать корпус посещения.
func (app *App) askZone(ctx context.Context, bctx BotContext) error {
	rows, err := app.zoneButtons()
	if err != nil {
		return err
	}
	rows = append(rows, []Button{btn("Другое", "draft:zone_custom", "")})
	rows = append(rows, []Button{btn("Назад", "draft:back_time", ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, "Выберите корпус/зону посещения.", rows)
}

// askZoneForEdit просит новый корпус при редактировании анкеты.
func (app *App) askZoneForEdit(ctx context.Context, bctx BotContext) error {
	rows, err := app.zoneButtonsWithAction("set_zone_summary")
	if err != nil {
		return err
	}
	rows = append(rows, []Button{btn("Другое", "draft:zone_custom_summary", "")})
	rows = append(rows, []Button{btn("Назад", "draft:edit", ""), btn("Главное меню", "menu", "")})
	return app.reply(ctx, bctx, "Выберите новый корпус/зону посещения.", rows)
}

// showDraftSummary показывает сводку анкеты перед отправкой.
func (app *App) showDraftSummary(ctx context.Context, bctx BotContext, user UserRow) error {
	draft, err := app.getDraft(user.ID)
	if err != nil {
		return err
	}
	if draft == nil {
		return app.reply(ctx, bctx, "Черновик не найден.", app.mainMenu(user))
	}
	zone := "не указано"
	if draft.ZoneID.Valid {
		zone = app.zoneName(draft.ZoneID.Int64)
	} else if draft.CustomZoneText.Valid {
		zone = draft.CustomZoneText.String
	}
	text := strings.Join([]string{
		"Проверьте заявку:",
		"",
		"ФИО: " + nullText(draft.FullName),
		"Дата: " + formatDate(nullText(draft.VisitDate)),
		"Время: " + nullText(draft.VisitTime),
		"Корпус/зона: " + zone,
		"Цель: " + nullText(draft.VisitPurpose),
	}, "\n")
	if extra := formatExtraFieldsForText(draft.ExtraFieldsJSON.String); extra != "" {
		text += "\nДоп. поля:\n" + extra
	}
	return app.reply(ctx, bctx, text, [][]Button{
		{btn("Отправить на рассмотрение", "draft:submit", "positive")},
		{btn("Изменить", "draft:edit", "")},
		{btn("Сохранить черновик", "draft:save", ""), btn("Удалить", "draft:delete", "negative")},
	})
}

// handleRequestCallback обрабатывает действия гостя с его заявкой.
func (app *App) handleRequestCallback(ctx context.Context, bctx BotContext, user UserRow, action, id string) error {
	requestID, _ := strconv.ParseInt(id, 10, 64)
	req, err := app.requestByID(requestID)
	if err != nil {
		return err
	}
	if req == nil {
		return app.reply(ctx, bctx, "Заявка не найдена.", mainMenuRows())
	}
	if req.UserID != user.ID && !app.isAdmin(user) {
		return app.reply(ctx, bctx, "Недоступно.", mainMenuRows())
	}
	switch action {
	case "open":
		return app.showRequestCard(ctx, bctx, user, *req)
	case "history":
		return app.showHistory(ctx, bctx, requestID)
	case "entries":
		return app.showEntries(ctx, bctx, requestID)
	case "cancel":
		if req.UserID != user.ID || (req.Status != "pending_review" && req.Status != "clarification_requested") {
			return app.reply(ctx, bctx, "Эту заявку уже нельзя отменить.", mainMenuRows())
		}
		if err := app.updateRequestStatus(requestID, "cancelled_by_initiator", user, "Заявка отменена инициатором.", ""); err != nil {
			return err
		}
		return app.reply(ctx, bctx, "Заявка отменена.", [][]Button{{btn("Мои заявки", "my:list", "")}})
	case "answer_clarification":
		app.setSession(user.MaxUserID, "initiator_clarify_answer", map[string]string{"request_id": id})
		question := "уточните данные заявки"
		if req.PublicComment.Valid {
			question = req.PublicComment.String
		}
		return app.reply(ctx, bctx, "Напишите ответ на уточнение одним сообщением.\n\nВопрос: "+question, [][]Button{
			{btn("Назад", fmt.Sprintf("request:open:%d", requestID), ""), btn("Главное меню", "menu", "")},
		})
	}
	return app.reply(ctx, bctx, "Неизвестное действие заявки.", mainMenuRows())
}

// handleAdminCallback обрабатывает действия администратора с заявками.
func (app *App) handleAdminCallback(ctx context.Context, bctx BotContext, user UserRow, action, id, extra string) error {
	if !app.isAdmin(user) {
		return app.reply(ctx, bctx, "Недоступно.", mainMenuRows())
	}
	switch action {
	case "menu":
		return app.adminMenu(ctx, bctx)
	case "queue":
		return app.adminQueue(ctx, bctx, "pending_review", parseInt(id))
	case "approved_today":
		return app.adminQueue(ctx, bctx, "approved_today", parseInt(id))
	case "search":
		app.setSession(user.MaxUserID, "admin_search", nil)
		return app.reply(ctx, bctx, "Введите номер заявки.", adminBackRows())
	case "export_active_today":
		return app.exportActiveToday(ctx, bctx)
	}

	requestID := parseInt64(id)
	switch action {
	case "approve":
		return app.reply(ctx, bctx, "Одобрить заявку?", [][]Button{
			{btn("Одобрить без комментария", fmt.Sprintf("admin:approve_now:%d", requestID), "positive")},
			{btn("Добавить комментарий", fmt.Sprintf("admin:approve_comment:%d", requestID), "")},
			{btn("Назад", fmt.Sprintf("request:open:%d", requestID), ""), btn("Меню админа", "admin:menu", "")},
		})
	case "approve_now":
		return app.approveRequest(ctx, bctx, user, requestID, "")
	case "approve_comment":
		app.setSession(user.MaxUserID, "admin_approve_comment", map[string]string{"request_id": id})
		return app.reply(ctx, bctx, "Введите комментарий к одобрению одним коротким сообщением.", adminBackRows())
	case "reject":
		return app.reply(ctx, bctx, "Выберите причину отклонения.", [][]Button{
			{btn("Недостаточно данных", fmt.Sprintf("admin:reject_reason:%d:Недостаточно данных", requestID), "")},
			{btn("Неверная дата визита", fmt.Sprintf("admin:reject_reason:%d:Неверная дата визита", requestID), "")},
			{btn("Неверно указана зона", fmt.Sprintf("admin:reject_reason:%d:Неверно указана зона", requestID), "")},
			{btn("Цель не подтверждена", fmt.Sprintf("admin:reject_reason:%d:Цель визита не подтверждена", requestID), "")},
			{btn("Другая причина", fmt.Sprintf("admin:reject_other:%d", requestID), "")},
			{btn("Меню админа", "admin:menu", ""), btn("Главное меню", "menu", "")},
		})
	case "reject_reason":
		reason := cleanAdminComment(extra)
		if reason == "" {
			return app.reply(ctx, bctx, "Причина отклонения не распознана.", adminBackRows())
		}
		if err := app.updateRequestStatus(requestID, "rejected", user, "Заявка отклонена. Причина: "+reason, reason); err != nil {
			return err
		}
		app.notifyOwner(ctx, requestID, "Ваша заявка отклонена.\nПричина: "+reason)
		return app.reply(ctx, bctx, "Заявка отклонена.", adminBackRows())
	case "reject_other":
		app.setSession(user.MaxUserID, "admin_reject_comment", map[string]string{"request_id": id})
		return app.reply(ctx, bctx, "Введите короткую причину отклонения.", adminBackRows())
	case "clarify":
		app.setSession(user.MaxUserID, "admin_clarify", map[string]string{"request_id": id})
		return app.reply(ctx, bctx, "Напишите вопросы для уточнения одним сообщением.", adminBackRows())
	case "entry":
		entryType, err := app.registerEntry(requestID, user)
		if err != nil {
			return app.reply(ctx, bctx, err.Error(), adminBackRows())
		}
		if entryType == "first_entry" {
			return app.reply(ctx, bctx, "Первый проход подтверждён.", adminBackRows())
		}
		return app.reply(ctx, bctx, "Повторный проход подтверждён.", adminBackRows())
	case "close":
		if err := app.updateRequestStatus(requestID, "closed", user, "Заявка закрыта.", ""); err != nil {
			return err
		}
		return app.reply(ctx, bctx, "Заявка закрыта.", adminBackRows())
	}
	return app.reply(ctx, bctx, "Неизвестное действие администратора.", adminBackRows())
}

// handleTechCallback обрабатывает действия техадмина.
func (app *App) handleTechCallback(ctx context.Context, bctx BotContext, user UserRow, action, id string) error {
	if !app.isTechAdmin(user) {
		return app.reply(ctx, bctx, "Недоступно.", mainMenuRows())
	}
	switch action {
	case "menu":
		return app.techMenu(ctx, bctx)
	case "admins":
		return app.showAdmins(ctx, bctx, roleAdmin)
	case "tech_admins":
		return app.showAdmins(ctx, bctx, roleTechAdmin)
	case "admin":
		return app.showAdminDetails(ctx, bctx, parseInt64(id))
	case "admin_audit":
		return app.showAdminAudit(ctx, bctx, parseInt64(id))
	case "revoke_admin":
		return app.revokeAdmin(ctx, bctx, user, parseInt64(id))
	case "zones":
		return app.showZones(ctx, bctx)
	case "extra_fields":
		return app.showExtraFields(ctx, bctx)
	case "toggle_zone":
		zoneID := parseInt64(id)
		var active int
		if err := app.queryRow(`SELECT is_active FROM zones WHERE id = ?`, zoneID).Scan(&active); err != nil {
			return app.reply(ctx, bctx, "Зона не найдена.", techBackRows())
		}
		next := 1
		if active == 1 {
			next = 0
		}
		if _, err := app.exec(`UPDATE zones SET is_active = ? WHERE id = ?`, next, zoneID); err != nil {
			return err
		}
		app.audit(user.MaxUserID, "zone_toggled", "zone", zoneID, map[string]string{"active": strconv.Itoa(next)})
		return app.showZones(ctx, bctx)
	case "add_zone":
		app.setSession(user.MaxUserID, "tech_zone_name", nil)
		return app.reply(ctx, bctx, "Введите короткое название зоны.", techBackRows())
	case "add_extra_fields":
		app.setSession(user.MaxUserID, "tech_extra_fields_add", nil)
		return app.reply(ctx, bctx, "Напишите названия дополнительных полей. Можно сразу несколько: каждое с новой строки или через запятую.", techBackRows())
	case "toggle_extra_field":
		return app.toggleExtraField(ctx, bctx, user, parseInt64(id))
	case "grant_admin":
		app.setSession(user.MaxUserID, "tech_grant_admin", nil)
		return app.reply(ctx, bctx, "Введите MAX user id пользователя, которому нужно выдать роль администратора.", techBackRows())
	}
	return app.reply(ctx, bctx, "Неизвестное действие техадмина.", techBackRows())
}

// handleSessionText принимает текст, который бот ждал на текущем шаге.
func (app *App) handleSessionText(ctx context.Context, bctx BotContext, user UserRow, session Session) error {
	text := strings.TrimSpace(bctx.Text)
	if text == "" {
		return app.reply(ctx, bctx, "Пришлите текстовое значение.", mainMenuRows())
	}

	switch session.State {
	case "draft_full_name":
		if errText := validateFullName(text); errText != "" {
			back := "menu"
			if session.Data["mode"] == "edit" {
				back = "draft:edit"
			}
			return app.reply(ctx, bctx, errText, draftBackRows(back))
		}
		app.clearSession(user.MaxUserID)
		if err := app.ensureDraft(user.ID); err != nil {
			return err
		}
		if err := app.updateDraft(user.ID, map[string]interface{}{"full_name": normalizeSpaces(text)}); err != nil {
			return err
		}
		if session.Data["mode"] == "edit" {
			return app.showDraftSummary(ctx, bctx, user)
		}
		return app.askDate(ctx, bctx, user)
	case "draft_date_custom":
		date := parseDateInput(text)
		if date == "" {
			return app.reply(ctx, bctx, "Дата должна быть в формате ДД.ММ.ГГГГ.", draftBackRows(dateInputBackPayload(session)))
		}
		if err := validateDate(date); err != nil {
			return app.reply(ctx, bctx, err.Error(), draftBackRows(dateInputBackPayload(session)))
		}
		app.clearSession(user.MaxUserID)
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_date": date}); err != nil {
			return err
		}
		if session.Data["mode"] == "edit" {
			return app.showDraftSummary(ctx, bctx, user)
		}
		return app.askTime(ctx, bctx)
	case "draft_time_custom":
		if errText := validateTime(text); errText != "" {
			return app.reply(ctx, bctx, errText, draftBackRows(timeInputBackPayload(session)))
		}
		if errText := app.validateDraftVisitTime(user.ID, text); errText != "" {
			return app.reply(ctx, bctx, errText, draftBackRows(timeInputBackPayload(session)))
		}
		app.clearSession(user.MaxUserID)
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_time": text}); err != nil {
			return err
		}
		if session.Data["mode"] == "edit" {
			return app.showDraftSummary(ctx, bctx, user)
		}
		return app.askZone(ctx, bctx)
	case "draft_purpose":
		if errText := validatePurpose(text); errText != "" {
			back := "draft:back_zone"
			if session.Data["mode"] == "edit" {
				back = "draft:edit"
			}
			return app.reply(ctx, bctx, errText, draftBackRows(back))
		}
		app.clearSession(user.MaxUserID)
		if err := app.updateDraft(user.ID, map[string]interface{}{"visit_purpose": text}); err != nil {
			return err
		}
		if session.Data["mode"] == "edit" {
			return app.showDraftSummary(ctx, bctx, user)
		}
		return app.askNextExtraFieldOrSummary(ctx, bctx, user)
	case "draft_extra_field":
		app.clearSession(user.MaxUserID)
		fieldID := parseInt64(session.Data["field_id"])
		label := session.Data["label"]
		if label == "" || fieldID <= 0 {
			return app.reply(ctx, bctx, "Не удалось распознать дополнительное поле. Вернитесь к черновику.", draftBackRows("draft:summary"))
		}
		if len([]rune(text)) > 240 {
			return app.reply(ctx, bctx, "Ответ должен быть не длиннее 240 символов.", draftBackRows("draft:back_purpose"))
		}
		if err := app.setDraftExtraField(user.ID, fieldID, label, text); err != nil {
			return err
		}
		return app.askNextExtraFieldOrSummary(ctx, bctx, user)
	case "admin_search":
		app.clearSession(user.MaxUserID)
		req, err := app.requestByNumber(text)
		if err != nil {
			return err
		}
		if req == nil {
			return app.reply(ctx, bctx, "Заявка не найдена.", adminBackRows())
		}
		return app.showRequestCard(ctx, bctx, user, *req)
	case "admin_approve_comment":
		app.clearSession(user.MaxUserID)
		requestID := parseInt64(session.Data["request_id"])
		comment := cleanAdminComment(text)
		if comment == "" {
			return app.reply(ctx, bctx, "Комментарий не должен быть пустым.", adminBackRows())
		}
		return app.approveRequest(ctx, bctx, user, requestID, comment)
	case "admin_reject_comment":
		app.clearSession(user.MaxUserID)
		requestID := parseInt64(session.Data["request_id"])
		reason := cleanAdminComment(text)
		if reason == "" {
			return app.reply(ctx, bctx, "Причина отклонения не должна быть пустой.", adminBackRows())
		}
		if err := app.updateRequestStatus(requestID, "rejected", user, "Заявка отклонена. Причина: "+reason, reason); err != nil {
			return err
		}
		app.notifyOwner(ctx, requestID, "Ваша заявка отклонена.\nПричина: "+reason)
		return app.reply(ctx, bctx, "Заявка отклонена.", adminBackRows())
	case "admin_clarify":
		app.clearSession(user.MaxUserID)
		requestID := parseInt64(session.Data["request_id"])
		if err := app.updateRequestStatus(requestID, "clarification_requested", user, "Запрошено уточнение: "+text, text); err != nil {
			return err
		}
		app.notifyOwnerWithRows(ctx, requestID, "По вашей заявке запрошено уточнение.\n\nВопрос: "+text+"\n\nНажмите кнопку ниже, чтобы отправить ответ одним сообщением.", [][]Button{
			{btn("Ответить на уточнение", fmt.Sprintf("request:answer_clarification:%d", requestID), "positive")},
			{btn("Мои заявки", "my:list", "")},
		})
		return app.reply(ctx, bctx, "Уточнение запрошено.", adminBackRows())
	case "initiator_clarify_answer":
		app.clearSession(user.MaxUserID)
		requestID := parseInt64(session.Data["request_id"])
		if _, err := app.exec(`UPDATE pass_requests SET status = 'pending_review', public_comment = NULL, updated_at = ? WHERE id = ?`, nowISO(), requestID); err != nil {
			return err
		}
		app.requestEvent(requestID, &user.ID, "clarification_answered", "Ответ инициатора: "+text)
		app.audit(user.MaxUserID, "clarification_answered", "pass_request", requestID, nil)
		return app.reply(ctx, bctx, "Ответ сохранён. Заявка вернулась на рассмотрение.", [][]Button{{btn("Мои заявки", "my:list", "")}})
	case "tech_zone_name":
		app.setSession(user.MaxUserID, "tech_zone_address", map[string]string{"short_name": text})
		return app.reply(ctx, bctx, "Введите адрес зоны.", techBackRows())
	case "tech_zone_address":
		shortName := session.Data["short_name"]
		app.clearSession(user.MaxUserID)
		code := slug(shortName)
		var sortOrder int
		_ = app.queryRow(`SELECT COALESCE(MAX(sort_order), 0) + 1 FROM zones`).Scan(&sortOrder)
		if _, err := app.exec(`INSERT INTO zones (code, short_name, address, sort_order) VALUES (?, ?, ?, ?)`, code, shortName, text, sortOrder); err != nil {
			return err
		}
		app.audit(user.MaxUserID, "zone_added", "zone", 0, map[string]string{"short_name": shortName})
		return app.reply(ctx, bctx, "Зона добавлена.", app.mainMenu(user))
	case "tech_grant_admin":
		app.clearSession(user.MaxUserID)
		return app.grantAdminFromText(ctx, bctx, user, text)
	case "tech_extra_fields_add":
		app.clearSession(user.MaxUserID)
		return app.addExtraFieldsFromText(ctx, bctx, user, text)
	}

	return app.reply(ctx, bctx, "Не удалось обработать ввод. Используйте /reset_session.", mainMenuRows())
}

// tryRecoverDraftInput подхватывает ввод анкеты, если сессия не успела сохраниться.
func (app *App) tryRecoverDraftInput(ctx context.Context, bctx BotContext, user UserRow) (bool, error) {
	if bctx.Text == "" {
		return false, nil
	}
	// У MAX callback и следующий текст иногда приходят с заметной задержкой.
	// Если сессия уже потерялась, но черновик явно ждёт следующее поле,
	// продолжаем сценарий вместо холодного "Откройте меню /start".
	draft, err := app.getDraft(user.ID)
	if err != nil || draft == nil {
		return false, err
	}
	if !draft.FullName.Valid {
		return true, app.handleSessionText(ctx, bctx, user, Session{State: "draft_full_name", Data: map[string]string{}})
	}
	if !draft.VisitDate.Valid {
		if parseDateInput(bctx.Text) != "" {
			return true, app.handleSessionText(ctx, bctx, user, Session{State: "draft_date_custom", Data: map[string]string{}})
		}
		return true, app.askDate(ctx, bctx, user)
	}
	if !draft.VisitTime.Valid {
		if validateTime(bctx.Text) == "" {
			return true, app.handleSessionText(ctx, bctx, user, Session{State: "draft_time_custom", Data: map[string]string{}})
		}
		return true, app.askTime(ctx, bctx)
	}
	if !draft.ZoneID.Valid && !draft.CustomZoneText.Valid {
		return true, app.askZone(ctx, bctx)
	}
	if !draft.VisitPurpose.Valid {
		return true, app.handleSessionText(ctx, bctx, user, Session{State: "draft_purpose", Data: map[string]string{}})
	}
	return false, nil
}
