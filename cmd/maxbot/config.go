package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// loadDotEnv читает локальный .env без внешних библиотек, чтобы запуск был простым.
func loadDotEnv(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimPrefix(strings.TrimSpace(raw), "\ufeff")
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
		if current, exists := os.LookupEnv(key); !exists || strings.TrimSpace(current) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}

// loadConfig собирает настройки бота из переменных окружения.
func loadConfig() Config {
	return Config{
		Token:         os.Getenv("BOT_TOKEN"),
		DBDriver:      strings.ToLower(getEnv("DB_DRIVER", "postgres")),
		DataDir:       getEnv("DATA_DIR", "./data"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		PolicyVersion: getEnv("POLICY_VERSION", "personal-data-v1"),
		MetricsAddr:   getEnv("METRICS_ADDR", ":8080"),
		AdminIDs:      parseIDSet(os.Getenv("ADMIN_USER_IDS")),
		TechAdminIDs:  parseIDSet(os.Getenv("TECH_ADMIN_USER_IDS")),
	}
}

// openDatabase открывает PostgreSQL и сразу проверяет соединение.
func openDatabase(cfg Config) (*sql.DB, error) {
	driver := strings.ToLower(strings.TrimSpace(cfg.DBDriver))
	if driver == "" {
		driver = "postgres"
	}
	switch driver {
	case "postgres", "postgresql":
		if strings.TrimSpace(cfg.DatabaseURL) == "" {
			return nil, errors.New("DATABASE_URL is empty for PostgreSQL mode")
		}
		return sql.Open("pgx", cfg.DatabaseURL)
	default:
		return nil, fmt.Errorf("unsupported DB_DRIVER %q", cfg.DBDriver)
	}
}

// getEnv берет значение из окружения, а если его нет — возвращает дефолт.
func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// parseIDSet разбирает список MAX user id из .env.
func parseIDSet(value string) map[int64]bool {
	result := map[int64]bool{}
	for _, part := range strings.Split(value, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && id > 0 {
			result[id] = true
		}
	}
	return result
}
