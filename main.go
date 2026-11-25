package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"time"
)

//go:embed web/*
var webFS embed.FS

type App struct {
	cfg     Config
	db      *sql.DB
	schema  DBSchema
	limiter *loginLimiter
}

func main() {
	cfg := loadConfig()

	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("Gagal terhubung ke database (%s@%s:%s/%s): %v",
			cfg.DBUser, cfg.DBHost, cfg.DBPort, cfg.DBName, err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	schema, err := detectSchema(ctx, db, cfg.DBSchemaMode)
	if err != nil {
		cancel()
		log.Fatalf("Deteksi skema database gagal: %v", err)
	}
	if err := migrateAttendanceTables(ctx, db, schema); err != nil {
		cancel()
		log.Fatalf("Migrasi gagal: %v", err)
	}
	cancel()
	log.Printf("Terhubung ke database %s (mode skema: %s, tabel siswa: %s) dan tabel absensi siap",
		cfg.DBName, schema.Mode, schema.StudentsTable)

	app := &App{cfg: cfg, db: db, schema: schema, limiter: newLoginLimiter()}
	mux := app.routes()

	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:              addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("Aplikasi Absensi QR & RFID berjalan di http://localhost%s (Kiosk: /, Admin: /admin)", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server berhenti: %v", err)
	}
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		if err := a.db.PingContext(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "database unreachable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db": a.cfg.DBName, "schema": a.schema.Mode})
	})

	// School branding & logo (public)
	mux.HandleFunc("GET /api/school/info", a.handleSchoolInfo)
	mux.HandleFunc("GET /api/school/logo", a.handleSchoolLogo)

	// Kiosk & IoT APIs
	mux.HandleFunc("POST /api/kiosk/scan", a.handleKioskScan)
	mux.HandleFunc("POST /api/iot/scan", a.handleIoTScan)
	mux.HandleFunc("GET /api/kiosk/recent", a.handleKioskRecent)

	// Auth APIs (mendukung skema revamp unbk_* maupun legacy tb_*)
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/auth/me", a.handleMe)

	// Admin / Guru APIs
	mux.HandleFunc("GET /api/admin/classes", a.requireAuth(a.handleAdminClasses))
	mux.HandleFunc("GET /api/admin/students", a.requireAuth(a.handleAdminStudents))
	mux.HandleFunc("POST /api/admin/students/card", a.requireAuth(a.handleAdminSaveCard))
	mux.HandleFunc("POST /api/admin/students/generate-qr", a.requireAuth(a.handleAdminBulkGenerateQR))
	mux.HandleFunc("GET /api/admin/unregistered-taps", a.requireAuth(a.handleAdminUnregisteredTaps))
	mux.HandleFunc("GET /api/admin/qr/{id}", a.requireAuth(a.handleAdminStudentQRPNG))
	mux.HandleFunc("GET /api/admin/attendance", a.requireAuth(a.handleAdminAttendance))
	mux.HandleFunc("POST /api/admin/attendance/manual", a.requireAuth(a.handleAdminManualAttendance))
	mux.HandleFunc("GET /api/admin/export", a.requireAuth(a.handleAdminExportCSV))
	mux.HandleFunc("GET /api/admin/settings", a.requireAuth(a.handleAdminGetSettings))
	mux.HandleFunc("POST /api/admin/settings", a.requireAdmin(a.handleAdminSaveSettings))
	mux.HandleFunc("POST /api/admin/logo", a.requireAdmin(a.handleAdminUploadLogo))
	mux.HandleFunc("DELETE /api/admin/logo", a.requireAdmin(a.handleAdminResetLogo))

	// Static & HTML pages
	staticSub, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", fileServer))

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveEmbeddedHTML(w, "web/kiosk.html")
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, "web/admin.html")
	})
	mux.HandleFunc("GET /print-qr", func(w http.ResponseWriter, r *http.Request) {
		serveEmbeddedHTML(w, "web/print_qr.html")
	})

	return mux
}

func serveEmbeddedHTML(w http.ResponseWriter, name string) {
	b, err := webFS.ReadFile(name)
	if err != nil {
		http.Error(w, "Halaman tidak ditemukan", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}
