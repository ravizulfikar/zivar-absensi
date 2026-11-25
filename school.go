package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) handleSchoolInfo(w http.ResponseWriter, r *http.Request) {
	s := loadSettings(r.Context(), a.db)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"nama_aplikasi":   s.NamaAplikasi,
		"nama_sekolah":    s.NamaSekolah,
		"alamat_sekolah":  s.AlamatSekolah,
		"telepon_sekolah": s.TeleponSekolah,
		"logo_url":        s.LogoURL,
		"schema":          a.schema.Mode,
	})
}

func (a *App) handleSchoolLogo(w http.ResponseWriter, r *http.Request) {
	if a.db != nil {
		s := loadSettings(r.Context(), a.db)
		if s.LogoFile != "" && !strings.ContainsAny(s.LogoFile, `/\`) && filepath.Base(s.LogoFile) == s.LogoFile {
			p := filepath.Join("storage", s.LogoFile)
			if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
				w.Header().Set("Content-Type", http.DetectContentType(data))
				w.Header().Set("Cache-Control", "public, max-age=3600")
				_, _ = w.Write(data)
				return
			}
		}
	}
	fallback, err := webFS.ReadFile("web/logo.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(fallback)
}

func (a *App) handleAdminUploadLogo(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Berkas logo tidak valid."})
		return
	}
	file, _, err := r.FormFile("logo")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Pilih file gambar logo terlebih dahulu."})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal membaca file logo."})
		return
	}
	if len(data) == 0 || len(data) > 2<<20 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Ukuran logo maksimal 2 MB."})
		return
	}

	mime := http.DetectContentType(data)
	extMap := map[string]string{
		"image/png":  ".png",
		"image/jpeg": ".jpg",
		"image/webp": ".webp",
		"image/gif":  ".gif",
	}
	ext, ok := extMap[mime]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Format logo harus PNG, JPG, WebP, atau GIF."})
		return
	}

	if err := os.MkdirAll("storage", 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyiapkan folder penyimpanan logo."})
		return
	}
	rnd := make([]byte, 8)
	_, _ = rand.Read(rnd)
	filename := "school_logo_" + hex.EncodeToString(rnd) + ext
	target := filepath.Join("storage", filename)
	if err := os.WriteFile(target, data, 0644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan file logo."})
		return
	}

	old := loadSettings(r.Context(), a.db).LogoFile
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO tb_absensi_pengaturan (kunci, nilai) VALUES ('logo_file', ?)
		 ON DUPLICATE KEY UPDATE nilai = VALUES(nilai)`, filename); err != nil {
		_ = os.Remove(target)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan pengaturan logo."})
		return
	}
	if old != "" && old != filename && filepath.Base(old) == old {
		_ = os.Remove(filepath.Join("storage", old))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"logo_url": "/api/school/logo?v=" + url.QueryEscape(filename),
	})
}

func (a *App) handleAdminResetLogo(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	old := loadSettings(r.Context(), a.db).LogoFile
	if _, err := a.db.ExecContext(r.Context(),
		`INSERT INTO tb_absensi_pengaturan (kunci, nilai) VALUES ('logo_file', '')
		 ON DUPLICATE KEY UPDATE nilai = ''`); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal mengatur ulang logo."})
		return
	}
	if old != "" && filepath.Base(old) == old {
		_ = os.Remove(filepath.Join("storage", old))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"logo_url": "/api/school/logo",
	})
}
