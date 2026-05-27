package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// newMetrics создает набор счетчиков для Prometheus.
func newMetrics() *Metrics {
	return &Metrics{
		startedAt:    time.Now(),
		updatesTotal: map[string]*atomic.Uint64{},
	}
}

// incUpdate увеличивает счетчик событий нужного типа.
func (m *Metrics) incUpdate(updateType string) {
	if m == nil {
		return
	}
	counter := m.updateCounter(updateType)
	counter.Add(1)
}

// observeUpdateLag обновляет текущую и максимальную задержку событий MAX.
func (m *Metrics) observeUpdateLag(lag time.Duration) {
	if m == nil || lag < 0 {
		return
	}
	millis := uint64(lag.Milliseconds())
	m.lastUpdateLagMillis.Store(millis)
	for {
		current := m.maxUpdateLagMillis.Load()
		if millis <= current || m.maxUpdateLagMillis.CompareAndSwap(current, millis) {
			return
		}
	}
}

// updateCounter лениво создает счетчик для нового типа события MAX.
func (m *Metrics) updateCounter(updateType string) *atomic.Uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.TrimSpace(updateType) == "" {
		updateType = "unknown"
	}
	counter, ok := m.updatesTotal[updateType]
	if !ok {
		counter = &atomic.Uint64{}
		m.updatesTotal[updateType] = counter
	}
	return counter
}

// updateSnapshot снимает копию счетчиков без долгой блокировки.
func (m *Metrics) updateSnapshot() map[string]uint64 {
	result := map[string]uint64{}
	if m == nil {
		return result
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for updateType, counter := range m.updatesTotal {
		result[updateType] = counter.Load()
	}
	return result
}

// exec выполняет SQL-команду через общий адаптер плейсхолдеров.
func (app *App) exec(query string, args ...interface{}) (sql.Result, error) {
	return app.db.Exec(app.sql(query), args...)
}

// query выполняет SQL-запрос, который возвращает несколько строк.
func (app *App) query(query string, args ...interface{}) (*sql.Rows, error) {
	return app.db.Query(app.sql(query), args...)
}

// queryRow выполняет SQL-запрос, который должен вернуть одну строку.
func (app *App) queryRow(query string, args ...interface{}) *sql.Row {
	return app.db.QueryRow(app.sql(query), args...)
}

// sql адаптирует общий SQL-синтаксис под PostgreSQL.
func (app *App) sql(query string) string {
	return rebindPostgres(query)
}

// rebindPostgres заменяет ? на $1, $2 и не трогает строки внутри SQL.
func rebindPostgres(query string) string {
	var out strings.Builder
	out.Grow(len(query) + 8)
	arg := 1
	inSingleQuote := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			out.WriteByte(ch)
			if inSingleQuote && i+1 < len(query) && query[i+1] == '\'' {
				i++
				out.WriteByte(query[i])
				continue
			}
			inSingleQuote = !inSingleQuote
			continue
		}
		if ch == '?' && !inSingleQuote {
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(arg))
			arg++
			continue
		}
		out.WriteByte(ch)
	}
	return out.String()
}

// serveMonitoring поднимает healthcheck и Prometheus-метрики.
func (app *App) serveMonitoring(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.healthHandler)
	mux.HandleFunc("/readyz", app.healthHandler)
	mux.HandleFunc("/metrics", app.metricsHandler)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("monitoring server listening on %s", addr)
	return server.ListenAndServe()
}

// healthHandler быстро проверяет, живы ли бот и база данных.
func (app *App) healthHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := app.db.PingContext(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("db_unavailable\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// metricsHandler отдает Prometheus-метрики наружу.
func (app *App) metricsHandler(w http.ResponseWriter, r *http.Request) {
	metrics, err := app.renderMetrics(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(metrics))
}

// renderMetrics собирает текст всех метрик приложения.
func (app *App) renderMetrics(ctx context.Context) (string, error) {
	var out strings.Builder
	metricStart(&out, "maxbot_build_info", "Static bot build information", "gauge")
	metricLine(&out, "maxbot_build_info", map[string]string{"app": "spring_code_1"}, 1)

	uptime := 0.0
	if app.metrics != nil && !app.metrics.startedAt.IsZero() {
		uptime = time.Since(app.metrics.startedAt).Seconds()
	}
	metricStart(&out, "maxbot_uptime_seconds", "Bot process uptime in seconds", "gauge")
	metricLine(&out, "maxbot_uptime_seconds", nil, uptime)

	if app.metrics != nil {
		metricStart(&out, "maxbot_updates_total", "Incoming MAX updates handled by type", "counter")
		for updateType, value := range app.metrics.updateSnapshot() {
			metricLine(&out, "maxbot_updates_total", map[string]string{"type": updateType, "type_label": updateTypeLabel(updateType)}, float64(value))
		}
		metricStart(&out, "maxbot_last_poll_duration_seconds", "Last MAX long polling request duration in seconds", "gauge")
		metricLine(&out, "maxbot_last_poll_duration_seconds", nil, float64(app.metrics.lastPollDurationMillis.Load())/1000)
		metricStart(&out, "maxbot_last_update_batch_size", "Number of updates in the last MAX polling response", "gauge")
		metricLine(&out, "maxbot_last_update_batch_size", nil, float64(app.metrics.lastBatchSize.Load()))
		metricStart(&out, "maxbot_last_update_lag_seconds", "Lag between MAX update timestamp and bot handling time in seconds", "gauge")
		metricLine(&out, "maxbot_last_update_lag_seconds", nil, float64(app.metrics.lastUpdateLagMillis.Load())/1000)
		metricStart(&out, "maxbot_max_update_lag_seconds", "Maximum observed lag between MAX update timestamp and bot handling time in seconds since process start", "gauge")
		metricLine(&out, "maxbot_max_update_lag_seconds", nil, float64(app.metrics.maxUpdateLagMillis.Load())/1000)
		metricStart(&out, "maxbot_update_errors_total", "Incoming MAX updates that returned an error", "counter")
		metricLine(&out, "maxbot_update_errors_total", nil, float64(app.metrics.updateErrors.Load()))
		metricStart(&out, "maxbot_poll_errors_total", "MAX long polling errors", "counter")
		metricLine(&out, "maxbot_poll_errors_total", nil, float64(app.metrics.pollErrorsTotal.Load()))
		metricStart(&out, "maxbot_replies_total", "Bot replies sent or recorded in tests", "counter")
		metricLine(&out, "maxbot_replies_total", nil, float64(app.metrics.repliesTotal.Load()))
	}

	stats := app.db.Stats()
	metricStart(&out, "maxbot_db_open_connections", "Open database connections", "gauge")
	metricLine(&out, "maxbot_db_open_connections", nil, float64(stats.OpenConnections))
	metricStart(&out, "maxbot_db_in_use_connections", "Database connections currently in use", "gauge")
	metricLine(&out, "maxbot_db_in_use_connections", nil, float64(stats.InUse))
	metricStart(&out, "maxbot_db_idle_connections", "Idle database connections", "gauge")
	metricLine(&out, "maxbot_db_idle_connections", nil, float64(stats.Idle))

	if err := app.appendCountMetric(ctx, &out, "maxbot_users_total", "Users known to the bot", `SELECT COUNT(*) FROM users`, nil); err != nil {
		return "", err
	}
	if err := app.appendCountMetric(ctx, &out, "maxbot_entry_events_total", "Entry events recorded by the bot", `SELECT COUNT(*) FROM entry_events`, nil); err != nil {
		return "", err
	}
	if err := app.appendCountMetric(ctx, &out, "maxbot_audit_events_total", "Audit events recorded by the bot", `SELECT COUNT(*) FROM audit_log`, nil); err != nil {
		return "", err
	}
	if err := app.appendRequestStatusMetrics(ctx, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// appendCountMetric добавляет в Prometheus одну числовую метрику из БД.
func (app *App) appendCountMetric(ctx context.Context, out *strings.Builder, name, help, query string, labels map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var count int64
	if err := app.db.QueryRowContext(ctx, app.sql(query)).Scan(&count); err != nil {
		return err
	}
	metricStart(out, name, help, "gauge")
	metricLine(out, name, labels, float64(count))
	return nil
}

// appendRequestStatusMetrics выводит количество заявок по статусам.
func (app *App) appendRequestStatusMetrics(ctx context.Context, out *strings.Builder) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := app.db.QueryContext(ctx, app.sql(`SELECT status, COUNT(*) FROM pass_requests GROUP BY status ORDER BY status`))
	if err != nil {
		return err
	}
	defer rows.Close()
	metricStart(out, "maxbot_pass_requests_total", "Pass requests by current status", "gauge")
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return err
		}
		metricLine(out, "maxbot_pass_requests_total", map[string]string{"status": status, "status_label": statusLabel(status)}, float64(count))
	}
	return rows.Err()
}

// metricStart пишет заголовок HELP/TYPE для метрики.
func metricStart(out *strings.Builder, name, help, metricType string) {
	out.WriteString("# HELP ")
	out.WriteString(name)
	out.WriteByte(' ')
	out.WriteString(help)
	out.WriteByte('\n')
	out.WriteString("# TYPE ")
	out.WriteString(name)
	out.WriteByte(' ')
	out.WriteString(metricType)
	out.WriteByte('\n')
}

// metricLine пишет одну строку значения метрики Prometheus.
func metricLine(out *strings.Builder, name string, labels map[string]string, value float64) {
	out.WriteString(name)
	if len(labels) > 0 {
		out.WriteByte('{')
		keys := make([]string, 0, len(labels))
		for key := range labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			value := labels[key]
			out.WriteString(key)
			out.WriteString(`="`)
			out.WriteString(escapeMetricLabel(value))
			out.WriteByte('"')
		}
		out.WriteByte('}')
	}
	out.WriteByte(' ')
	out.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	out.WriteByte('\n')
}

// escapeMetricLabel экранирует кавычки и переносы в label Prometheus.
func escapeMetricLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return value
}

// updateTypeLabel переводит технический тип MAX-события в понятное имя.
func updateTypeLabel(updateType string) string {
	labels := map[string]string{
		"message_created":  "сообщения пользователей",
		"message_callback": "нажатия кнопок",
		"bot_started":      "первый запуск бота",
		"unknown":          "неизвестные события",
	}
	if label, ok := labels[updateType]; ok {
		return label
	}
	return updateType
}
