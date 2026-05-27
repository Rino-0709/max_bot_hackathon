package main

import (
	"context"
	"errors"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	if err := loadDotEnv(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("load .env: %v", err)
	}

	cfg := loadConfig()
	if cfg.Token == "" {
		log.Fatal("BOT_TOKEN is empty")
	}

	db, err := openDatabase(cfg)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	app := &App{
		cfg:       cfg,
		db:        db,
		metrics:   newMetrics(),
		lastStart: map[int64]time.Time{},
		api: &MaxAPI{
			token:   cfg.Token,
			baseURL: "https://platform-api.max.ru",
			client:  &http.Client{Timeout: 45 * time.Second},
		},
	}

	if err := app.initDB(); err != nil {
		log.Fatalf("init db: %v", err)
	}
	if err := app.expireOldRequests(context.Background()); err != nil {
		log.Printf("expire old requests on startup: %v", err)
	}
	if cfg.MetricsAddr != "" {
		go func() {
			if err := app.serveMonitoring(context.Background(), cfg.MetricsAddr); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("monitoring server stopped: %v", err)
			}
		}()
	}

	log.Println("Весенний_код_1 Go bot started")
	if err := app.poll(context.Background()); err != nil {
		log.Fatalf("polling stopped: %v", err)
	}
}
