package storage

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/audit-agent/pkg/audit"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db    *sql.DB
	mu    sync.RWMutex
	path  string
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}

	store := &SQLiteStore{
		db:   db,
		path: path,
	}

	if err := store.init(); err != nil {
		db.Close()
		return nil, err
	}

	return store, nil
}

func (s *SQLiteStore) init() error {
	schema := `
	CREATE TABLE IF NOT EXISTS records (
		id TEXT PRIMARY KEY,
		message_id TEXT UNIQUE NOT NULL,
		session_id TEXT NOT NULL,
		seq_num INTEGER NOT NULL,
		timestamp TEXT NOT NULL,
		type INTEGER NOT NULL,
		is_input INTEGER NOT NULL DEFAULT 0,
		data TEXT NOT NULL,
		status INTEGER NOT NULL DEFAULT 0,
		retry_cnt INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_records_session_id ON records(session_id);
	CREATE INDEX IF NOT EXISTS idx_records_status ON records(status);
	CREATE INDEX IF NOT EXISTS idx_records_seq_num ON records(session_id, seq_num);

	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		pid INTEGER,
		shell TEXT NOT NULL,
		start_time TEXT NOT NULL,
		end_time TEXT,
		rows INTEGER NOT NULL DEFAULT 40,
		cols INTEGER NOT NULL DEFAULT 120,
		hostname TEXT,
		username TEXT
	);
	`

	_, err := s.db.Exec(schema)
	return err
}

func (s *SQLiteStore) SaveRecord(record *audit.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `INSERT INTO records (id, message_id, session_id, seq_num, timestamp, type, is_input, data, status, retry_cnt, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.Exec(query,
		record.ID,
		record.MessageID,
		record.SessionID,
		record.SeqNum,
		record.Timestamp.Format(time.RFC3339Nano),
		record.Type,
		record.IsInput,
		record.Data,
		record.Status,
		record.RetryCnt,
		record.CreatedAt.Format(time.RFC3339Nano),
	)

	return err
}

func (s *SQLiteStore) GetPendingRecords(limit int) ([]audit.Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, message_id, session_id, seq_num, timestamp, type, is_input, data, status, retry_cnt, created_at
		FROM records WHERE status IN (?, ?) ORDER BY seq_num ASC LIMIT ?`

	rows, err := s.db.Query(query, audit.StatusPending, audit.StatusFailed, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []audit.Record
	for rows.Next() {
		var r audit.Record
		var timestamp, createdAt string
		err := rows.Scan(&r.ID, &r.MessageID, &r.SessionID, &r.SeqNum, &timestamp, &r.Type, &r.IsInput, &r.Data, &r.Status, &r.RetryCnt, &createdAt)
		if err != nil {
			return nil, err
		}

		r.Timestamp, _ = time.Parse(time.RFC3339Nano, timestamp)
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		records = append(records, r)
	}

	return records, rows.Err()
}

func (s *SQLiteStore) UpdateRecordStatus(messageID string, status audit.RecordStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `UPDATE records SET status = ? WHERE message_id = ?`
	_, err := s.db.Exec(query, status, messageID)
	return err
}

func (s *SQLiteStore) IncrementRetryCount(messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `UPDATE records SET retry_cnt = retry_cnt + 1 WHERE message_id = ?`
	_, err := s.db.Exec(query, messageID)
	return err
}

func (s *SQLiteStore) SaveSession(info *audit.SessionInfo) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `INSERT OR REPLACE INTO sessions (id, pid, shell, start_time, end_time, rows, cols, hostname, username)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var endTime *string
	if !info.EndTime.IsZero() {
		t := info.EndTime.Format(time.RFC3339Nano)
		endTime = &t
	}

	_, err := s.db.Exec(query,
		info.ID,
		info.PID,
		info.Shell,
		info.StartTime.Format(time.RFC3339Nano),
		endTime,
		info.Rows,
		info.Cols,
		info.Hostname,
		info.Username,
	)

	return err
}

func (s *SQLiteStore) GetSession(id string) (*audit.SessionInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, pid, shell, start_time, end_time, rows, cols, hostname, username FROM sessions WHERE id = ?`

	var info audit.SessionInfo
	var pid *int
	var startTime, endTime string

	err := s.db.QueryRow(query, id).Scan(&info.ID, &pid, &info.Shell, &startTime, &endTime, &info.Rows, &info.Cols, &info.Hostname, &info.Username)
	if err != nil {
		return nil, err
	}

	info.PID = -1
	if pid != nil {
		info.PID = *pid
	}

	info.StartTime, _ = time.Parse(time.RFC3339Nano, startTime)
	if endTime != "" {
		info.EndTime, _ = time.Parse(time.RFC3339Nano, endTime)
	}

	return &info, nil
}

func (s *SQLiteStore) GetRecordsBySession(sessionID string) ([]audit.Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, message_id, session_id, seq_num, timestamp, type, is_input, data, status, retry_cnt, created_at
		FROM records WHERE session_id = ? ORDER BY seq_num ASC`

	rows, err := s.db.Query(query, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []audit.Record
	for rows.Next() {
		var r audit.Record
		var timestamp, createdAt string
		err := rows.Scan(&r.ID, &r.MessageID, &r.SessionID, &r.SeqNum, &timestamp, &r.Type, &r.IsInput, &r.Data, &r.Status, &r.RetryCnt, &createdAt)
		if err != nil {
			return nil, err
		}

		r.Timestamp, _ = time.Parse(time.RFC3339Nano, timestamp)
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		records = append(records, r)
	}

	return records, rows.Err()
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) GetDB() *sql.DB {
	return s.db
}

func (s *SQLiteStore) GetStats() (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make(map[string]int)

	for _, status := range []audit.RecordStatus{audit.StatusPending, audit.StatusUploading, audit.StatusUploaded, audit.StatusFailed} {
		var count int
		query := `SELECT COUNT(*) FROM records WHERE status = ?`
		err := s.db.QueryRow(query, status).Scan(&count)
		if err != nil {
			return nil, err
		}
		stats[fmt.Sprintf("status_%d", status)] = count
	}

	var totalRecords int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&totalRecords)
	if err != nil {
		return nil, err
	}
	stats["total"] = totalRecords

	var totalSessions int
	err = s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&totalSessions)
	if err != nil {
		return nil, err
	}
	stats["sessions"] = totalSessions

	return stats, nil
}
