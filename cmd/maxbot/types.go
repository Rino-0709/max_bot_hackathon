package main

import (
	"database/sql"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	roleInitiator = "initiator"
	roleAdmin     = "admin"
	roleTechAdmin = "tech_admin"
)

var initialZones = []struct {
	Code      string
	ShortName string
	Address   string
}{
	{"vernadskogo-78", "Проспект Вернадского, 78", "г. Москва, проспект Вернадского, 78"},
	{"vernadskogo-86", "Проспект Вернадского, 86", "г. Москва, проспект Вернадского, 86"},
	{"stromynka-20", "Стромынка, 20", "г. Москва, ул. Стромынка, 20"},
	{"pirogovskaya-1-5", "Малая Пироговская, 1 стр. 5", "г. Москва, ул. Малая Пироговская, 1, стр. 5"},
	{"sokolinoy-gory-22", "5-я Соколиной Горы, 22", "г. Москва, ул. 5-я Соколиной Горы, 22"},
	{"shchipkovskiy-23", "1-й Щипковский, 23", "г. Москва, 1-й Щипковский пер., 23"},
	{"usacheva-7-1", "Усачева, 7/1", "г. Москва, ул. Усачева, 7/1"},
}

type Config struct {
	Token         string
	DBDriver      string
	DataDir       string
	DatabaseURL   string
	PolicyVersion string
	MetricsAddr   string
	QRSecret      string
	ScannerToken  string
	AdminIDs      map[int64]bool
	TechAdminIDs  map[int64]bool
}

type App struct {
	cfg         Config
	db          *sql.DB
	api         *MaxAPI
	metrics     *Metrics
	testReplies []TestReply
	startMu     sync.Mutex
	lastStart   map[int64]time.Time
}

type Metrics struct {
	startedAt              time.Time
	updatesTotal           map[string]*atomic.Uint64
	updateErrors           atomic.Uint64
	repliesTotal           atomic.Uint64
	pollErrorsTotal        atomic.Uint64
	lastPollDurationMillis atomic.Uint64
	lastBatchSize          atomic.Uint64
	lastUpdateLagMillis    atomic.Uint64
	maxUpdateLagMillis     atomic.Uint64
	mu                     sync.Mutex
}

type TestReply struct {
	Text string
	Rows [][]Button
}

type MaxAPI struct {
	token   string
	baseURL string
	client  *http.Client
}

type UpdateResponse struct {
	Updates []Update `json:"updates"`
	Marker  int64    `json:"marker"`
}

type Update struct {
	UpdateType string    `json:"update_type"`
	Timestamp  int64     `json:"timestamp"`
	Message    *Message  `json:"message,omitempty"`
	Callback   *Callback `json:"callback,omitempty"`
	User       *MaxUser  `json:"user,omitempty"`
	ChatID     int64     `json:"chat_id,omitempty"`
	Payload    string    `json:"payload,omitempty"`
}

type Callback struct {
	Timestamp  int64   `json:"timestamp"`
	CallbackID string  `json:"callback_id"`
	Payload    string  `json:"payload"`
	User       MaxUser `json:"user"`
}

type Message struct {
	Sender    *MaxUser `json:"sender,omitempty"`
	Recipient struct {
		ChatID   int64  `json:"chat_id"`
		ChatType string `json:"chat_type"`
	} `json:"recipient"`
	Body struct {
		MID  string `json:"mid"`
		Text string `json:"text"`
	} `json:"body"`
}

type MaxUser struct {
	UserID   int64   `json:"user_id"`
	Name     string  `json:"name"`
	Username *string `json:"username"`
	IsBot    bool    `json:"is_bot"`
}

type Button struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
	Intent  string `json:"intent,omitempty"`
}

type Attachment struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload"`
}

type SendMessageBody struct {
	Text        string       `json:"text,omitempty"`
	Format      string       `json:"format,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

type UploadEndpoint struct {
	Token string `json:"token,omitempty"`
	URL   string `json:"url"`
}

type UploadedInfo struct {
	FileID int64  `json:"file_id,omitempty"`
	Token  string `json:"token,omitempty"`
}

type UserRow struct {
	ID          int64
	MaxUserID   int64
	DisplayName string
	Role        string
}

type DraftRow struct {
	ID              int64
	UserID          int64
	FullName        sql.NullString
	VisitDate       sql.NullString
	VisitTime       sql.NullString
	ZoneID          sql.NullInt64
	CustomZoneText  sql.NullString
	VisitPurpose    sql.NullString
	ExtraFieldsJSON sql.NullString
}

type ZoneRow struct {
	ID        int64
	Code      string
	ShortName string
	Address   string
	IsActive  int
	SortOrder int
}

type RequestRow struct {
	ID              int64
	RequestNumber   string
	UserID          int64
	FullName        string
	VisitDate       string
	VisitTime       string
	ZoneID          sql.NullInt64
	CustomZoneText  sql.NullString
	VisitPurpose    string
	ExtraFieldsJSON sql.NullString
	Status          string
	PublicComment   sql.NullString
	DisplayName     sql.NullString
	MaxUserID       sql.NullInt64
	ZoneName        sql.NullString
	ZoneAddress     sql.NullString
	CreatedAt       string
	UpdatedAt       string
}

type ExtraFieldRow struct {
	ID        int64
	Label     string
	IsActive  int
	SortOrder int
}

type ExtraFieldValue struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
}

type ExportRequestRow struct {
	Number    string
	FullName  string
	Date      string
	Time      string
	Zone      string
	Purpose   string
	Extra     string
	Status    string
	Comment   string
	UpdatedAt string
}

type Session struct {
	State string
	Data  map[string]string
}

type BotContext struct {
	User       MaxUser
	Text       string
	Payload    string
	CallbackID string
}
