package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

func (a *App) handleAdminClasses(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	rows, err := a.db.QueryContext(r.Context(),
		`SELECT id_kelas, nama_kelas FROM `+a.schema.ClassesTable+` ORDER BY nama_kelas ASC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memuat daftar kelas"})
		return
	}
	defer rows.Close()

	type classItem struct {
		IDKelas   int    `json:"id_kelas"`
		NamaKelas string `json:"nama_kelas"`
	}
	out := make([]classItem, 0, 32)
	for rows.Next() {
		var c classItem
		if err := rows.Scan(&c.IDKelas, &c.NamaKelas); err == nil {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "classes": out})
}

func (a *App) handleAdminStudents(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	kelas := strings.TrimSpace(r.URL.Query().Get("kelas"))
	filter := strings.TrimSpace(r.URL.Query().Get("filter"))

	where := []string{"1=1"}
	args := []any{}

	if kelas != "" {
		where = append(where, "TRIM(s.id_kelas) = ?")
		args = append(args, kelas)
	}
	if q != "" {
		like := "%" + q + "%"
		where = append(where, "(s.nama_lengkap LIKE ? OR s.nis LIKE ? OR COALESCE(c.rfid_uid,'') LIKE ? OR COALESCE(c.qr_token,'') LIKE ?)")
		args = append(args, like, like, like, like)
	}
	switch filter {
	case "no_rfid":
		where = append(where, "(c.rfid_uid IS NULL OR c.rfid_uid = '')")
	case "has_rfid":
		where = append(where, "(c.rfid_uid IS NOT NULL AND c.rfid_uid <> '')")
	case "no_qr":
		where = append(where, "(c.qr_token IS NULL OR c.qr_token = '')")
	case "aktif":
		where = append(where, "s.status = 'aktif'")
	}

	sqlQuery := `
		SELECT s.id_siswa, COALESCE(s.nis,''), COALESCE(s.nama_lengkap,''),
		       COALESCE(s.jenis_kelamin,'L'), TRIM(COALESCE(s.id_kelas,'')),
		       COALESCE(k.nama_kelas,'-'), COALESCE(s.status,'aktif'),
		       COALESCE(c.rfid_uid,''), COALESCE(c.qr_token,''), COALESCE(c.status_kartu,'aktif')
		FROM ` + a.schema.StudentsTable + ` s
		LEFT JOIN ` + a.schema.ClassesTable + ` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
		LEFT JOIN tb_absensi_kartu c ON c.id_siswa = s.id_siswa
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY k.nama_kelas ASC, s.nama_lengkap ASC
		LIMIT 5000`

	rows, err := a.db.QueryContext(r.Context(), sqlQuery, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memuat data siswa"})
		return
	}
	defer rows.Close()

	type studentRow struct {
		IDSiswa     int    `json:"id_siswa"`
		NIS         string `json:"nis"`
		Nama        string `json:"nama"`
		JK          string `json:"jk"`
		IDKelas     string `json:"id_kelas"`
		NamaKelas   string `json:"nama_kelas"`
		StatusSiswa string `json:"status_siswa"`
		RFIDUID     string `json:"rfid_uid"`
		QRToken     string `json:"qr_token"`
		StatusKartu string `json:"status_kartu"`
	}

	list := make([]studentRow, 0, 64)
	for rows.Next() {
		var s studentRow
		if err := rows.Scan(&s.IDSiswa, &s.NIS, &s.Nama, &s.JK, &s.IDKelas, &s.NamaKelas, &s.StatusSiswa, &s.RFIDUID, &s.QRToken, &s.StatusKartu); err == nil {
			list = append(list, s)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "students": list})
}

func (a *App) handleAdminSaveCard(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	var req struct {
		IDSiswa     int    `json:"id_siswa"`
		RFIDUID     string `json:"rfid_uid"`
		QRToken     string `json:"qr_token"`
		StatusKartu string `json:"status_kartu"`
		Regenerate  bool   `json:"regenerate_qr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IDSiswa <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Data siswa tidak valid"})
		return
	}

	ctx := r.Context()
	var nis string
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(nis,'') FROM `+a.schema.StudentsTable+` WHERE id_siswa = ? LIMIT 1`, req.IDSiswa).Scan(&nis); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Siswa tidak ditemukan di database"})
		return
	}

	rfid := normalizeRFID(req.RFIDUID)
	var rfidArg any
	if rfid == "" {
		rfidArg = nil
	} else {
		// Cek bentrok RFID dengan siswa lain
		var ownerID int
		var ownerName string
		err := a.db.QueryRowContext(ctx,
			`SELECT c.id_siswa, COALESCE(s.nama_lengkap,'') FROM tb_absensi_kartu c
			 INNER JOIN `+a.schema.StudentsTable+` s ON s.id_siswa = c.id_siswa
			 WHERE REPLACE(REPLACE(REPLACE(UPPER(c.rfid_uid),':',''),'-',''),' ','') = ? AND c.id_siswa <> ? LIMIT 1`, rfid, req.IDSiswa).Scan(&ownerID, &ownerName)
		if err == nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok":    false,
				"error": fmt.Sprintf("UID RFID %s sudah terdaftar atas nama %s", rfid, ownerName),
			})
			return
		}
		rfidArg = rfid
	}

	qrToken := strings.TrimSpace(req.QRToken)
	if req.Regenerate || qrToken == "" {
		if !req.Regenerate {
			var existingQR string
			if err := a.db.QueryRowContext(ctx, `SELECT qr_token FROM tb_absensi_kartu WHERE id_siswa = ?`, req.IDSiswa).Scan(&existingQR); err == nil && existingQR != "" {
				qrToken = existingQR
			}
		}
		if qrToken == "" || req.Regenerate {
			qrToken = generateQRToken(nis)
		}
	}

	statusKartu := "aktif"
	if strings.EqualFold(req.StatusKartu, "nonaktif") {
		statusKartu = "nonaktif"
	}

	_, err := a.db.ExecContext(ctx, `
		INSERT INTO tb_absensi_kartu (id_siswa, rfid_uid, qr_token, status_kartu, updated_at)
		VALUES (?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			rfid_uid = VALUES(rfid_uid),
			qr_token = VALUES(qr_token),
			status_kartu = VALUES(status_kartu),
			updated_at = NOW()`,
		req.IDSiswa, rfidArg, qrToken, statusKartu)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan kartu siswa: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"id_siswa":     req.IDSiswa,
		"rfid_uid":     rfid,
		"qr_token":     qrToken,
		"status_kartu": statusKartu,
	})
}

func (a *App) handleAdminBulkGenerateQR(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	ctx := r.Context()
	rows, err := a.db.QueryContext(ctx, `
		SELECT s.id_siswa, COALESCE(s.nis,'')
		FROM `+a.schema.StudentsTable+` s
		LEFT JOIN tb_absensi_kartu c ON c.id_siswa = s.id_siswa
		WHERE s.status = 'aktif' AND (c.qr_token IS NULL OR c.qr_token = '')`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memeriksa siswa tanpa QR"})
		return
	}
	defer rows.Close()

	type pair struct {
		id  int
		nis string
	}
	var pending []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.nis); err == nil {
			pending = append(pending, p)
		}
	}

	created := 0
	for _, p := range pending {
		if _, err := ensureStudentQRToken(ctx, a.db, p.id, p.nis); err == nil {
			created++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"created": created,
		"pesan":   fmt.Sprintf("Berhasil membuat QR Code statis untuk %d siswa aktif", created),
	})
}

func (a *App) handleAdminUnregisteredTaps(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT l.kode, l.metode, l.kiosk, MAX(l.waktu) AS terakhir
		FROM tb_absensi_log l
		LEFT JOIN tb_absensi_kartu c ON UPPER(c.rfid_uid) = UPPER(l.kode)
		WHERE l.hasil = 'tidak_dikenal' AND l.metode = 'rfid' AND c.id_siswa IS NULL
		GROUP BY l.kode, l.metode, l.kiosk
		ORDER BY terakhir DESC
		LIMIT 10`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memuat log tap baru"})
		return
	}
	defer rows.Close()

	type tapItem struct {
		Kode     string `json:"kode"`
		Metode   string `json:"metode"`
		Kiosk    string `json:"kiosk"`
		Terakhir string `json:"terakhir"`
	}
	taps := make([]tapItem, 0, 10)
	for rows.Next() {
		var t tapItem
		var ts time.Time
		if err := rows.Scan(&t.Kode, &t.Metode, &t.Kiosk, &ts); err == nil {
			t.Terakhir = ts.In(a.cfg.Location).Format("2006-01-02 15:04:05")
			taps = append(taps, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "taps": taps})
}

func (a *App) handleAdminStudentQRPNG(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	idStr := r.PathValue("id")
	idSiswa, err := strconv.Atoi(idStr)
	if err != nil || idSiswa <= 0 {
		http.Error(w, "ID siswa tidak valid", http.StatusBadRequest)
		return
	}
	var nis string
	if err := a.db.QueryRowContext(r.Context(), `SELECT COALESCE(nis,'') FROM `+a.schema.StudentsTable+` WHERE id_siswa = ? LIMIT 1`, idSiswa).Scan(&nis); err != nil {
		http.Error(w, "Siswa tidak ditemukan", http.StatusNotFound)
		return
	}
	token, err := ensureStudentQRToken(r.Context(), a.db, idSiswa, nis)
	if err != nil {
		http.Error(w, "Gagal membuat token QR", http.StatusInternalServerError)
		return
	}
	png, err := qrcode.Encode(token, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "Gagal merender gambar QR", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (a *App) handleAdminAttendance(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	tanggal := strings.TrimSpace(r.URL.Query().Get("tanggal"))
	if tanggal == "" {
		tanggal = time.Now().In(a.cfg.Location).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", tanggal); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Format tanggal harus YYYY-MM-DD"})
		return
	}
	kelas := strings.TrimSpace(r.URL.Query().Get("kelas"))
	statusFilter := strings.TrimSpace(r.URL.Query().Get("status"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	where := []string{"s.status = 'aktif'"}
	args := []any{tanggal}

	if kelas != "" {
		where = append(where, "TRIM(s.id_kelas) = ?")
		args = append(args, kelas)
	}
	if q != "" {
		like := "%" + q + "%"
		where = append(where, "(s.nama_lengkap LIKE ? OR s.nis LIKE ?)")
		args = append(args, like, like)
	}
	if statusFilter != "" && statusFilter != "semua" {
		if statusFilter == "belum_absen" {
			where = append(where, "a.id IS NULL")
		} else {
			where = append(where, "a.status = ?")
			args = append(args, statusFilter)
		}
	}

	sqlQuery := `
		SELECT s.id_siswa, COALESCE(s.nis,''), COALESCE(s.nama_lengkap,''),
		       TRIM(COALESCE(s.id_kelas,'')), COALESCE(k.nama_kelas,'-'),
		       COALESCE(a.status, 'belum_absen'),
		       a.jam_masuk, a.jam_pulang,
		       COALESCE(a.metode_masuk,''), COALESCE(a.metode_pulang,''),
		       COALESCE(a.keterangan,'')
		FROM ` + a.schema.StudentsTable + ` s
		LEFT JOIN ` + a.schema.ClassesTable + ` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
		LEFT JOIN tb_absensi_harian a ON a.id_siswa = s.id_siswa AND a.tanggal = ?
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY k.nama_kelas ASC, s.nama_lengkap ASC
		LIMIT 5000`

	rows, err := a.db.QueryContext(r.Context(), sqlQuery, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memuat data absensi"})
		return
	}
	defer rows.Close()

	type attRow struct {
		IDSiswa      int    `json:"id_siswa"`
		NIS          string `json:"nis"`
		Nama         string `json:"nama"`
		IDKelas      string `json:"id_kelas"`
		NamaKelas    string `json:"nama_kelas"`
		Status       string `json:"status"`
		JamMasuk     string `json:"jam_masuk"`
		JamPulang    string `json:"jam_pulang"`
		MetodeMasuk  string `json:"metode_masuk"`
		MetodePulang string `json:"metode_pulang"`
		Keterangan   string `json:"keterangan"`
	}

	list := make([]attRow, 0, 128)
	counts := map[string]int{
		"total":       0,
		"hadir":       0,
		"terlambat":   0,
		"izin":        0,
		"sakit":       0,
		"alpha":       0,
		"belum_absen": 0,
	}

	for rows.Next() {
		var row attRow
		var jm, jp sql.NullTime
		if err := rows.Scan(&row.IDSiswa, &row.NIS, &row.Nama, &row.IDKelas, &row.NamaKelas,
			&row.Status, &jm, &jp, &row.MetodeMasuk, &row.MetodePulang, &row.Keterangan); err != nil {
			continue
		}
		if jm.Valid {
			row.JamMasuk = jm.Time.In(a.cfg.Location).Format("15:04:05")
		}
		if jp.Valid {
			row.JamPulang = jp.Time.In(a.cfg.Location).Format("15:04:05")
		}
		counts["total"]++
		counts[row.Status]++
		list = append(list, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"tanggal": tanggal,
		"counts":  counts,
		"rows":    list,
	})
}

func (a *App) handleAdminManualAttendance(w http.ResponseWriter, r *http.Request, user *SessionUser) {
	var req struct {
		IDSiswa    int    `json:"id_siswa"`
		Tanggal    string `json:"tanggal"`
		Status     string `json:"status"`
		JamMasuk   string `json:"jam_masuk"`
		JamPulang  string `json:"jam_pulang"`
		Keterangan string `json:"keterangan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IDSiswa <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Data absensi manual tidak valid"})
		return
	}
	if req.Tanggal == "" {
		req.Tanggal = time.Now().In(a.cfg.Location).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", req.Tanggal); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Format tanggal harus YYYY-MM-DD"})
		return
	}

	validStatus := map[string]bool{
		"hadir": true, "terlambat": true, "izin": true, "sakit": true, "alpha": true,
	}
	if !validStatus[req.Status] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Status harus hadir, terlambat, izin, sakit, atau alpha"})
		return
	}

	parseTimeOpt := func(hhmm string) any {
		hhmm = strings.TrimSpace(hhmm)
		if hhmm == "" {
			return nil
		}
		layout := "2006-01-02 15:04"
		if len(hhmm) == 8 {
			layout = "2006-01-02 15:04:05"
		}
		t, err := time.ParseInLocation(layout, req.Tanggal+" "+hhmm, a.cfg.Location)
		if err != nil {
			return nil
		}
		return t
	}

	jm := parseTimeOpt(req.JamMasuk)
	jp := parseTimeOpt(req.JamPulang)
	if (req.Status == "hadir" || req.Status == "terlambat") && jm == nil {
		jm = time.Now().In(a.cfg.Location)
	}
	isAbsent := req.Status == "izin" || req.Status == "sakit" || req.Status == "alpha"
	if isAbsent {
		jm = nil
		jp = nil
	}

	var metMasuk, metPulang any
	if jm != nil {
		metMasuk = "manual"
	}
	if jp != nil {
		metPulang = "manual"
	}

	now := time.Now().In(a.cfg.Location)
	var err error
	if isAbsent {
		_, err = a.db.ExecContext(r.Context(), `
			INSERT INTO tb_absensi_harian (id_siswa, tanggal, jam_masuk, jam_pulang, status, metode_masuk, metode_pulang, keterangan, updated_by, created_at, updated_at)
			VALUES (?, ?, NULL, NULL, ?, NULL, NULL, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				jam_masuk = NULL,
				jam_pulang = NULL,
				status = VALUES(status),
				metode_masuk = NULL,
				metode_pulang = NULL,
				keterangan = VALUES(keterangan),
				updated_by = VALUES(updated_by),
				updated_at = VALUES(updated_at)`,
			req.IDSiswa, req.Tanggal, req.Status, strings.TrimSpace(req.Keterangan), user.Username, now, now)
	} else {
		_, err = a.db.ExecContext(r.Context(), `
			INSERT INTO tb_absensi_harian (id_siswa, tanggal, jam_masuk, jam_pulang, status, metode_masuk, metode_pulang, keterangan, updated_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				jam_masuk = VALUES(jam_masuk),
				jam_pulang = VALUES(jam_pulang),
				status = VALUES(status),
				metode_masuk = COALESCE(tb_absensi_harian.metode_masuk, VALUES(metode_masuk)),
				metode_pulang = COALESCE(tb_absensi_harian.metode_pulang, VALUES(metode_pulang)),
				keterangan = VALUES(keterangan),
				updated_by = VALUES(updated_by),
				updated_at = VALUES(updated_at)`,
			req.IDSiswa, req.Tanggal, jm, jp, req.Status, metMasuk, metPulang, strings.TrimSpace(req.Keterangan), user.Username, now, now)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan absensi manual"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleAdminExportCSV(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	dari := strings.TrimSpace(r.URL.Query().Get("dari"))
	sampai := strings.TrimSpace(r.URL.Query().Get("sampai"))
	tanggal := strings.TrimSpace(r.URL.Query().Get("tanggal"))
	kelas := strings.TrimSpace(r.URL.Query().Get("kelas"))

	today := time.Now().In(a.cfg.Location).Format("2006-01-02")
	if dari == "" && sampai == "" {
		if tanggal == "" {
			tanggal = today
		}
		dari = tanggal
		sampai = tanggal
	}
	if sampai == "" {
		sampai = dari
	}
	if _, err := time.Parse("2006-01-02", dari); err != nil {
		http.Error(w, "Format tanggal 'dari' harus YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	if _, err := time.Parse("2006-01-02", sampai); err != nil {
		http.Error(w, "Format tanggal 'sampai' harus YYYY-MM-DD", http.StatusBadRequest)
		return
	}

	where := []string{"a.tanggal BETWEEN ? AND ?"}
	args := []any{dari, sampai}
	if kelas != "" {
		where = append(where, "TRIM(s.id_kelas) = ?")
		args = append(args, kelas)
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT DATE_FORMAT(a.tanggal, '%Y-%m-%d'), COALESCE(s.nis,''), COALESCE(s.nama_lengkap,''),
		       COALESCE(k.nama_kelas,'-'), a.status,
		       a.jam_masuk, a.jam_pulang,
		       COALESCE(a.metode_masuk,''), COALESCE(a.metode_pulang,''),
		       COALESCE(a.keterangan,'')
		FROM tb_absensi_harian a
		INNER JOIN `+a.schema.StudentsTable+` s ON s.id_siswa = a.id_siswa
		LEFT JOIN `+a.schema.ClassesTable+` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY a.tanggal ASC, k.nama_kelas ASC, s.nama_lengkap ASC`, args...)
	if err != nil {
		http.Error(w, "Gagal mengekspor data absensi", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	filename := fmt.Sprintf("rekap_absensi_%s_sd_%s.csv", dari, sampai)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	// UTF-8 BOM agar Microsoft Excel langsung membaca kolom & karakter dengan benar
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"Tanggal", "NIS", "Nama Siswa", "Kelas", "Status", "Jam Masuk", "Jam Pulang", "Metode Masuk", "Metode Pulang", "Keterangan"})

	for rows.Next() {
		var tgl, nis, nama, nmKelas, status, mMasuk, mPulang, ket string
		var jm, jp sql.NullTime
		if err := rows.Scan(&tgl, &nis, &nama, &nmKelas, &status, &jm, &jp, &mMasuk, &mPulang, &ket); err != nil {
			continue
		}
		jmStr, jpStr := "", ""
		if jm.Valid {
			jmStr = jm.Time.In(a.cfg.Location).Format("15:04:05")
		}
		if jp.Valid {
			jpStr = jp.Time.In(a.cfg.Location).Format("15:04:05")
		}
		_ = cw.Write([]string{tgl, nis, nama, nmKelas, strings.ToUpper(status), jmStr, jpStr, strings.ToUpper(mMasuk), strings.ToUpper(mPulang), ket})
	}
	cw.Flush()
}

func (a *App) handleAdminGetSettings(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"settings": loadSettings(r.Context(), a.db),
	})
}

func (a *App) handleAdminSaveSettings(w http.ResponseWriter, r *http.Request, _ *SessionUser) {
	var req Settings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Format pengaturan tidak valid"})
		return
	}
	if err := saveSettings(r.Context(), a.db, req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": loadSettings(r.Context(), a.db)})
}
