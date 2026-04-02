package audit

import (
	"time"
)

type RecordStatus int

const (
	StatusPending RecordStatus = iota
	StatusUploading
	StatusUploaded
	StatusFailed
)

type Record struct {
	ID        string       `json:"id"`
	MessageID string       `json:"message_id"`
	SessionID string       `json:"session_id"`
	SeqNum    int64        `json:"seq_num"`
	Timestamp time.Time    `json:"timestamp"`
	Type      RecordType   `json:"type"`
	IsInput   bool         `json:"is_input"` // true for input, false for output
	Data      string       `json:"data"`
	Status    RecordStatus `json:"status"`
	RetryCnt  int          `json:"retry_cnt"`
	CreatedAt time.Time    `json:"created_at"`
}

type RecordType int

const (
	TypeInput RecordType = iota
	TypeOutput
	TypeError
	TypeResize
	TypeExit
)

type SessionInfo struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	Shell     string    `json:"shell"`
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time,omitempty"`
	Rows      uint16    `json:"rows"`
	Cols      uint16    `json:"cols"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
}

type UploadRequest struct {
	MessageID string    `json:"message_id"`
	SessionID string    `json:"session_id"`
	Records   []Record  `json:"records"`
	Timestamp time.Time `json:"timestamp"`
}

type UploadResponse struct {
	Success   bool      `json:"success"`
	MessageID string    `json:"message_id"`
	Timestamp time.Time `json:"timestamp"`
	Error     string    `json:"error,omitempty"`
}
