package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/audit-agent/internal/storage"
	"github.com/audit-agent/pkg/audit"
)

type Reporter struct {
	store     *storage.SQLiteStore
	serverURL string
	client    *http.Client
	stopCh    chan struct{}
	wg        sync.WaitGroup
	isRunning bool
	mu        sync.RWMutex

	maxRetries  int
	batchSize   int
	workerCount int

	countThreshold int
	timeThreshold  time.Duration
	lastFlushTime  time.Time
	flushMu        sync.Mutex

	batchBuffer []*audit.Record
	bufferMu    sync.Mutex

	serverAvailable bool
	availableMu     sync.RWMutex

	dynamicBatch *DynamicBatcher
}

type DynamicBatcher struct {
	mu          sync.RWMutex
	baseSize    int
	currentSize int
	minSize     int
	maxSize     int

	pendingCount     int
	backlogThreshold int

	serverLatency    time.Duration
	latencyThreshold time.Duration
	lastLatencyTime  time.Time

	increaseFactor float64
	decreaseFactor float64
	adjustInterval time.Duration
	lastAdjustTime time.Time
}

func NewDynamicBatcher(baseSize, minSize, maxSize int) *DynamicBatcher {
	return &DynamicBatcher{
		baseSize:         baseSize,
		currentSize:      baseSize,
		minSize:          minSize,
		maxSize:          maxSize,
		backlogThreshold: 100,
		latencyThreshold: 500 * time.Millisecond,
		increaseFactor:   1.5,
		decreaseFactor:   0.7,
		adjustInterval:   10 * time.Second,
		lastAdjustTime:   time.Now(),
	}
}

func (db *DynamicBatcher) GetBatchSize() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.currentSize
}

func (db *DynamicBatcher) RecordLatency(latency time.Duration) {
	db.mu.Lock()
	defer db.mu.Unlock()

	db.serverLatency = latency
	db.lastLatencyTime = time.Now()
}

func (db *DynamicBatcher) RecordPending(count int) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.pendingCount = count
}

func (db *DynamicBatcher) Adjust() {
	db.mu.Lock()
	defer db.mu.Unlock()

	if time.Since(db.lastAdjustTime) < db.adjustInterval {
		return
	}
	db.lastAdjustTime = time.Now()

	oldSize := db.currentSize

	if db.pendingCount > db.backlogThreshold && db.serverLatency < db.latencyThreshold {
		newSize := int(float64(db.currentSize) * db.increaseFactor)
		if newSize > db.maxSize {
			newSize = db.maxSize
		}
		db.currentSize = newSize
		log.Printf("[DynamicBatch] Backlog high(%d>%d), latency low(%v), INCREASE batch: %d -> %d",
			db.pendingCount, db.backlogThreshold, db.serverLatency, oldSize, db.currentSize)
	} else if db.serverLatency > db.latencyThreshold {
		newSize := int(float64(db.currentSize) * db.decreaseFactor)
		if newSize < db.minSize {
			newSize = db.minSize
		}
		db.currentSize = newSize
		log.Printf("[DynamicBatch] Latency high(%v>%v), DECREASE batch: %d -> %d",
			db.serverLatency, db.latencyThreshold, oldSize, db.currentSize)
	}
}

func NewReporter(store *storage.SQLiteStore, serverURL string) *Reporter {
	dynamicBatch := NewDynamicBatcher(20, 5, 100)

	return &Reporter{
		store:           store,
		serverURL:       serverURL,
		client:          &http.Client{Timeout: 30 * time.Second},
		stopCh:          make(chan struct{}),
		maxRetries:      5,
		batchSize:       50,
		workerCount:     3,
		countThreshold:  20,
		timeThreshold:   5 * time.Second,
		lastFlushTime:   time.Now(),
		batchBuffer:     make([]*audit.Record, 0, 100),
		serverAvailable: false,
		dynamicBatch:    dynamicBatch,
	}
}

func (r *Reporter) Start() error {
	r.mu.Lock()
	if r.isRunning {
		r.mu.Unlock()
		return nil
	}
	r.isRunning = true
	r.mu.Unlock()

	r.wg.Add(1)
	go r.batchWorker()

	r.wg.Add(1)
	go r.retryWorker()

	r.wg.Add(1)
	go r.dynamicAdjustWorker()

	return nil
}

func (r *Reporter) Stop() {
	r.mu.Lock()
	if !r.isRunning {
		r.mu.Unlock()
		return
	}
	r.isRunning = false
	r.mu.Unlock()

	close(r.stopCh)
	r.wg.Wait()
}

func (r *Reporter) SubmitRecord(record *audit.Record) {
	log.Printf("[Reporter] SubmitRecord: MessageID=%s, SeqNum=%d", record.MessageID, record.SeqNum)

	if !r.isServerAvailable() {
		log.Printf("[Reporter] Server unavailable, record %s queued locally", record.MessageID)
		return
	}

	r.bufferMu.Lock()
	r.batchBuffer = append(r.batchBuffer, record)
	currentSize := len(r.batchBuffer)
	r.bufferMu.Unlock()

	log.Printf("[Reporter] Added to buffer, current size: %d", currentSize)

	r.dynamicBatch.RecordPending(currentSize)

	countThreshold := r.getCountThreshold()
	if currentSize >= countThreshold {
		log.Printf("[Reporter] Threshold reached, triggering flush")
		r.triggerFlush()
	}
}

func (r *Reporter) getCountThreshold() int {
	r.mu.RLock()
	defer r.mu.RUnlock()

	baseThreshold := r.countThreshold
	dynamicSize := r.dynamicBatch.GetBatchSize()

	if dynamicSize > baseThreshold {
		return dynamicSize
	}
	return baseThreshold
}

func (r *Reporter) isServerAvailable() bool {
	r.availableMu.RLock()
	defer r.availableMu.RUnlock()
	return r.serverAvailable
}

func (r *Reporter) SetServerAvailable(available bool) {
	r.availableMu.Lock()
	r.serverAvailable = available
	r.availableMu.Unlock()

	if available {
		log.Printf("[Reporter] Server is online, resuming uploads")
		go r.flushPendingRecords()
	} else {
		log.Printf("[Reporter] Server is offline, pausing uploads")
	}
}

func (r *Reporter) triggerFlush() {
	r.flushMu.Lock()
	if r.lastFlushTime.IsZero() {
		r.lastFlushTime = time.Now()
	}
	shouldFlush := time.Since(r.lastFlushTime) >= r.timeThreshold || len(r.batchBuffer) >= r.getCountThreshold()
	r.flushMu.Unlock()

	if shouldFlush {
		go r.doFlush()
	}
}

func (r *Reporter) batchWorker() {
	defer r.wg.Done()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			r.doFlush()
			return
		case <-ticker.C:
			r.dynamicBatch.Adjust()

			r.flushMu.Lock()
			shouldFlush := time.Since(r.lastFlushTime) >= r.timeThreshold
			r.flushMu.Unlock()

			if shouldFlush {
				r.doFlush()
			}
		}
	}
}

func (r *Reporter) dynamicAdjustWorker() {
	defer r.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.bufferMu.Lock()
			pending := len(r.batchBuffer)
			r.bufferMu.Unlock()

			r.dynamicBatch.RecordPending(pending)

			pendingRecords, _ := r.store.GetPendingRecords(1000)
			r.dynamicBatch.RecordPending(len(pendingRecords))
		}
	}
}

func (r *Reporter) doFlush() {
	r.bufferMu.Lock()
	if len(r.batchBuffer) == 0 {
		r.bufferMu.Unlock()
		return
	}

	dynamicSize := r.dynamicBatch.GetBatchSize()

	recordsToFlush := r.batchBuffer
	if len(recordsToFlush) > dynamicSize {
		recordsToFlush = r.batchBuffer[:dynamicSize]
		r.batchBuffer = r.batchBuffer[dynamicSize:]
	} else {
		r.batchBuffer = r.batchBuffer[:0]
	}
	r.bufferMu.Unlock()

	r.flushMu.Lock()
	r.lastFlushTime = time.Now()
	r.flushMu.Unlock()

	r.uploadBatch(recordsToFlush)
}

func (r *Reporter) uploadBatch(records []*audit.Record) {
	if len(records) == 0 {
		return
	}

	for _, record := range records {
		r.store.UpdateRecordStatus(record.MessageID, audit.StatusUploading)
	}

	recordPtrs := make([]audit.Record, len(records))
	for i, rec := range records {
		recordPtrs[i] = *rec
	}

	req := audit.UploadRequest{
		MessageID: records[0].MessageID,
		SessionID: records[0].SessionID,
		Records:   recordPtrs,
		Timestamp: time.Now(),
	}

	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("[Reporter] Failed to marshal request: %v", err)
		r.handleBatchError(records, err)
		return
	}

	startTime := time.Now()
	resp, err := r.client.Post(r.serverURL+"/api/v1/audit/upload", "application/json", bytes.NewBuffer(body))
	latency := time.Since(startTime)

	r.dynamicBatch.RecordLatency(latency)

	if err != nil {
		log.Printf("[Reporter] Failed to send batch: %v", err)
		r.SetServerAvailable(false)
		r.handleBatchError(records, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		log.Printf("[Reporter] Server returned %d: %s", resp.StatusCode, string(respBody))

		if resp.StatusCode >= 500 {
			r.SetServerAvailable(false)
		}

		r.handleBatchError(records, fmt.Errorf("server returned %d", resp.StatusCode))
		return
	}

	r.SetServerAvailable(true)

	var uploadResp audit.UploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&uploadResp); err != nil {
		log.Printf("[Reporter] Failed to decode response: %v", err)
		r.handleBatchError(records, err)
		return
	}

	if !uploadResp.Success {
		log.Printf("[Reporter] Upload failed: %s", uploadResp.Error)
		r.handleBatchError(records, fmt.Errorf("%s", uploadResp.Error))
		return
	}

	for _, record := range records {
		r.store.UpdateRecordStatus(record.MessageID, audit.StatusUploaded)
	}

	log.Printf("[Reporter] Successfully uploaded %d records (latency: %v, currentBatchSize: %d)",
		len(records), latency, r.dynamicBatch.GetBatchSize())
}

func (r *Reporter) handleBatchError(records []*audit.Record, err error) {
	for _, record := range records {
		record.RetryCnt++
		if record.RetryCnt >= r.maxRetries {
			r.store.UpdateRecordStatus(record.MessageID, audit.StatusFailed)
			log.Printf("[Reporter] Record %s failed after %d retries", record.MessageID, record.RetryCnt)
			continue
		}

		r.store.IncrementRetryCount(record.MessageID)
		r.store.UpdateRecordStatus(record.MessageID, audit.StatusPending)

		time.AfterFunc(time.Duration(record.RetryCnt*2)*time.Second, func() {
			r.bufferMu.Lock()
			r.batchBuffer = append(r.batchBuffer, record)
			r.bufferMu.Unlock()
		})
	}
}

func (r *Reporter) retryWorker() {
	defer r.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.retryPendingUploads()
		}
	}
}

func (r *Reporter) retryPendingUploads() {
	records, err := r.store.GetPendingRecords(r.batchSize)
	if err != nil {
		return
	}

	if len(records) == 0 {
		return
	}

	log.Printf("[Reporter] Retrying %d pending records", len(records))

	r.bufferMu.Lock()
	for i := range records {
		r.batchBuffer = append(r.batchBuffer, &records[i])
	}
	r.bufferMu.Unlock()

	r.doFlush()
}

func (r *Reporter) flushPendingRecords() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for {
		records, err := r.store.GetPendingRecords(r.batchSize)
		if err != nil {
			log.Printf("[Reporter] Failed to get pending records: %v", err)
			return
		}

		if len(records) == 0 {
			log.Printf("[Reporter] All pending records flushed")
			return
		}

		log.Printf("[Reporter] Flushing %d pending records after server recovery", len(records))

		recordsPtrs := make([]*audit.Record, len(records))
		for i := range records {
			recordsPtrs[i] = &records[i]
		}

		r.uploadBatch(recordsPtrs)

		select {
		case <-ctx.Done():
			log.Printf("[Reporter] Flush timeout")
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *Reporter) UploadPendingRecords(ctx context.Context) error {
	records, err := r.store.GetPendingRecords(r.batchSize)
	if err != nil {
		return err
	}

	if len(records) == 0 {
		return nil
	}

	recordsPtrs := make([]*audit.Record, len(records))
	for i := range records {
		recordsPtrs[i] = &records[i]
	}

	r.uploadBatch(recordsPtrs)
	return nil
}

func (r *Reporter) GetBufferSize() int {
	r.bufferMu.Lock()
	defer r.bufferMu.Unlock()
	return len(r.batchBuffer)
}

func (r *Reporter) GetDynamicBatchSize() int {
	return r.dynamicBatch.GetBatchSize()
}

func (r *Reporter) SetThresholds(count int, timeout time.Duration) {
	r.mu.Lock()
	r.countThreshold = count
	r.timeThreshold = timeout
	r.mu.Unlock()

	log.Printf("[Reporter] Thresholds updated: count=%d, timeout=%v", count, timeout)
}
