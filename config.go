package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"time"
)

type Config struct {
	Port          string
	DBHost        string
	DBPort        string
	DBUser        string
	DBPass        string
	DBName        string
	DBSchemaMode  string // "auto", "revamp", atau "legacy"
	SessionSecret string
	SecureCookie  bool
	Location      *time.Location
}

func loadConfig() Config {
	loadDotEnv(".env")

	tzName := getEnv("TZ", "Asia/Jakarta")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		// Fallback fixed UTC+7 (WIB) jika tzdata sistem tidak tersedia di Windows.
		loc = time.FixedZone("WIB", 7*3600)
	}
	time.Local = loc

	secret := getEnv("SESSION_SECRET", "")
	if secret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err == nil {
			secret = hex.EncodeToString(buf)
		} else {
			secret = "absensi55-fallback-secret-change-me"
		}
	}

	schemaMode := strings.ToLower(getEnv("DB_SCHEMA", "auto"))
	if schemaMode != "revamp" && schemaMode != "legacy" {
		schemaMode = "auto"
	}

	secureRaw := strings.ToLower(getEnv("SECURE_COOKIE", "0"))
	secureCookie := secureRaw == "1" || secureRaw == "true"

	return Config{
		Port:          getEnv("APP_PORT", "8090"),
		DBHost:        getEnv("DB_HOST", "127.0.0.1"),
		DBPort:        getEnv("DB_PORT", "3306"),
		DBUser:        getEnv("DB_USER", "root"),
		DBPass:        getEnv("DB_PASS", ""),
		DBName:        getEnv("DB_NAME", "db_ujian55"),
		DBSchemaMode:  schemaMode,
		SessionSecret: secret,
		SecureCookie:  secureCookie,
		Location:      loc,
	}
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		if k != "" && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}
