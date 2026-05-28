package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type rateBucket struct {
	ResetAt time.Time
	Count   int
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
}

// allow пропускает ограниченное число действий за окно времени.
func (limiter *RateLimiter) allow(key string, limit int, window time.Duration) bool {
	if limiter == nil {
		return true
	}
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if limiter.buckets == nil {
		limiter.buckets = map[string]rateBucket{}
	}
	now := time.Now()
	bucket := limiter.buckets[key]
	if now.After(bucket.ResetAt) {
		bucket = rateBucket{ResetAt: now.Add(window)}
	}
	bucket.Count++
	limiter.buckets[key] = bucket
	return bucket.Count <= limit
}

// scannerRateKey собирает ключ лимита из IP и начала ключа сканера.
func scannerRateKey(r *http.Request) string {
	ip := scannerClientIP(r)
	token := scannerTokenFromRequest(r)
	if len(token) > 24 {
		token = token[:24]
	}
	return ip + ":" + token
}

// scannerClientIP достает реальный IP с учетом nginx proxy headers.
func scannerClientIP(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Real-IP")); value != "" {
		return value
	}
	if value := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); value != "" {
		first, _, _ := strings.Cut(value, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
