package recorder

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/acarl005/stripansi"
	"github.com/audit-agent/internal/pty"
	"github.com/audit-agent/internal/storage"
	"github.com/audit-agent/pkg/audit"
	"github.com/audit-agent/pkg/idgen"
)

type Recorder struct {
	session    *pty.Session
	store      *storage.SQLiteStore
	sessionID  string
	seqNum     int64
	mu         sync.Mutex
	recordCh   chan *audit.Record
	uploadCh   chan *audit.Record
	outputCh   chan []byte
	stopCh     chan struct{}
	stoppedCh  chan struct{}
	wg         sync.WaitGroup
	isRunning  bool
	lastInput  []byte
}

func NewRecorder(session *pty.Session, store *storage.SQLiteStore, sessionID string) *Recorder {
	return &Recorder{
		session:   session,
		store:     store,
		sessionID: sessionID,
		seqNum:    0,
		recordCh:  make(chan *audit.Record, 1000),
		uploadCh:  make(chan *audit.Record, 1000),
		outputCh:  make(chan []byte, 1024*1024),
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

func (r *Recorder) Start() error {
	r.mu.Lock()
	if r.isRunning {
		r.mu.Unlock()
		return nil
	}
	r.isRunning = true
	r.mu.Unlock()

	if err := r.session.Start(); err != nil {
		return err
	}

	r.wg.Add(2)
	go r.recordLoop()
	go r.persistLoop()

	return nil
}

func (r *Recorder) recordLoop() {
	defer r.wg.Done()

	for {
		select {
		case <-r.stopCh:
			close(r.outputCh)
			return
		default:
		}

		data, err := r.session.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				log.Println("[Recorder] PTY EOF, exiting")
				close(r.outputCh)
				return
			}
			select {
			case <-r.stopCh:
				close(r.outputCh)
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}

		if len(data) > 0 {
			cleanedData := cleanData(data)

			if len(cleanedData) == 0 {
				continue
			}

			if r.isEcho(data) {
				continue
			}

			log.Printf("[Recorder] Received output: %d bytes, cleaned: %d bytes", len(data), len(cleanedData))
			r.recordData(cleanedData, audit.TypeOutput, false)
			select {
			case r.outputCh <- data:
			case <-r.stopCh:
				close(r.outputCh)
				return
			}
		}
	}
}

func (r *Recorder) isEcho(data []byte) bool {
	if len(r.lastInput) == 0 || len(data) == 0 {
		return false
	}

	cleanedInput := cleanData(r.lastInput)
	cleanedOutput := cleanData(data)

	if len(cleanedInput) == 0 || len(cleanedOutput) == 0 {
		return false
	}

	if strings.TrimSpace(string(cleanedInput)) == strings.TrimSpace(string(cleanedOutput)) {
		log.Printf("[Recorder] Detected echo, skipping duplicate")
		r.lastInput = nil
		return true
	}

	return false
}

func cleanData(data []byte) []byte {
	s := stripansi.Strip(string(data))
	s = strings.ReplaceAll(s, "\x00", "")

	var result strings.Builder
	for _, r := range s {
		if r == '\r' || r == '\n' {
			result.WriteRune(r)
		} else if r >= 32 || r == '\t' || unicode.IsPrint(r) {
			result.WriteRune(r)
		}
	}

	return []byte(result.String())
}

func isValidData(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	cleaned := cleanData(data)
	if len(cleaned) == 0 {
		return false
	}

	trimmed := strings.TrimSpace(string(cleaned))
	if len(trimmed) == 0 {
		return false
	}

	return true
}

func (r *Recorder) persistLoop() {
	defer r.wg.Done()

	batch := make([]*audit.Record, 0, 100)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			if len(batch) > 0 {
				r.persistBatch(batch)
			}
			return
		case record := <-r.recordCh:
			batch = append(batch, record)
			if len(batch) >= 100 {
				r.persistBatch(batch)
				batch = make([]*audit.Record, 0, 100)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				r.persistBatch(batch)
				batch = make([]*audit.Record, 0, 100)
			}
		}
	}
}

func (r *Recorder) persistBatch(batch []*audit.Record) {
	for _, record := range batch {
		if err := r.store.SaveRecord(record); err != nil {
			log.Printf("[Recorder] Failed to save record: %v", err)
			continue
		}
		select {
		case r.uploadCh <- record:
		default:
		}
	}
}

func (r *Recorder) RecordInput(data []byte) {
	if !isValidData(data) {
		return
	}

	r.mu.Lock()
	r.lastInput = data
	r.mu.Unlock()

	r.recordData(data, audit.TypeInput, true)
}

func (r *Recorder) recordData(data []byte, recType audit.RecordType, isInput bool) {
	r.mu.Lock()
	r.seqNum++
	seqNum := r.seqNum
	timestamp := time.Now()
	r.mu.Unlock()

	messageID := fmt.Sprintf("%d", idgen.GenerateMessageID())

	record := &audit.Record{
		ID:        messageID,
		MessageID: messageID,
		SessionID: r.sessionID,
		SeqNum:    seqNum,
		Timestamp: timestamp,
		Type:      recType,
		IsInput:   isInput,
		Data:      encodeData(data),
		Status:    audit.StatusPending,
		RetryCnt:  0,
		CreatedAt: timestamp,
	}

	select {
	case r.recordCh <- record:
	default:
		log.Printf("[Recorder] recordCh full, dropping record")
	}
}

func (r *Recorder) RecordResize(rows, cols uint16) {
	r.mu.Lock()
	r.seqNum++
	seqNum := r.seqNum
	timestamp := time.Now()
	r.mu.Unlock()

	data := encodeResize(rows, cols)
	messageID := fmt.Sprintf("%d", idgen.GenerateMessageID())

	record := &audit.Record{
		ID:        messageID,
		MessageID: messageID,
		SessionID: r.sessionID,
		SeqNum:    seqNum,
		Timestamp: timestamp,
		Type:      audit.TypeResize,
		IsInput:   false,
		Data:      data,
		Status:    audit.StatusPending,
		RetryCnt:  0,
		CreatedAt: timestamp,
	}

	select {
	case r.recordCh <- record:
	case <-r.stopCh:
	default:
	}
}

func (r *Recorder) RecordError(data []byte) {
	r.recordData(data, audit.TypeError, false)
}

func (r *Recorder) RecordExit(exitCode int) {
	r.mu.Lock()
	r.seqNum++
	seqNum := r.seqNum
	timestamp := time.Now()
	r.mu.Unlock()

	data := encodeExitCode(exitCode)
	messageID := fmt.Sprintf("%d", idgen.GenerateMessageID())

	record := &audit.Record{
		ID:        messageID,
		MessageID: messageID,
		SessionID: r.sessionID,
		SeqNum:    seqNum,
		Timestamp: timestamp,
		Type:      audit.TypeExit,
		IsInput:   false,
		Data:      data,
		Status:    audit.StatusPending,
		RetryCnt:  0,
		CreatedAt: timestamp,
	}

	select {
	case r.recordCh <- record:
	case <-r.stopCh:
	}

	r.Stop()
}

func (r *Recorder) Stop() {
	r.mu.Lock()
	if !r.isRunning {
		r.mu.Unlock()
		return
	}
	r.isRunning = false
	r.mu.Unlock()

	select {
	case <-r.stopCh:
		return
	default:
	}

	close(r.stopCh)

	go func() {
		r.wg.Wait()
		r.session.Close()
		close(r.stoppedCh)
	}()
}

func (r *Recorder) GetUploadChannel() <-chan *audit.Record {
	return r.uploadCh
}

func (r *Recorder) GetOutputChannel() <-chan []byte {
	return r.outputCh
}

func (r *Recorder) GetStoppedChannel() <-chan struct{} {
	return r.stoppedCh
}

func (r *Recorder) GetSessionID() string {
	return r.sessionID
}

func (r *Recorder) GetSeqNum() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seqNum
}

func encodeData(data []byte) string {
	return stripansi.Strip(string(data))
}

func decodeData(encoded string) []byte {
	return []byte(encoded)
}

func encodeResize(rows, cols uint16) string {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.BigEndian, rows)
	binary.Write(buf, binary.BigEndian, cols)
	return buf.String()
}

func decodeResize(encoded string) (rows, cols uint16, err error) {
	buf := bytes.NewBufferString(encoded)
	err = binary.Read(buf, binary.BigEndian, &rows)
	if err != nil {
		return 0, 0, err
	}
	err = binary.Read(buf, binary.BigEndian, &cols)
	return rows, cols, err
}

func encodeExitCode(code int) string {
	buf := new(bytes.Buffer)
	binary.Write(buf, binary.LittleEndian, int32(code))
	return buf.String()
}

func decodeExitCode(encoded string) (int, error) {
	buf := bytes.NewBufferString(encoded)
	var code int32
	err := binary.Read(buf, binary.LittleEndian, &code)
	return int(code), err
}

func (r *Recorder) Write(data []byte) (int, error) {
	if r.session == nil {
		return 0, io.EOF
	}
	r.RecordInput(data)
	return r.session.Write(data)
}
