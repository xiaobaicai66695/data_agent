package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/audit-agent/pkg/audit"
	_ "github.com/lib/pq"
	"github.com/joho/godotenv"
)

var (
	listenAddr string
	pgDSN      string
)

func init() {
	flag.StringVar(&listenAddr, "listen", getEnv("SERVER_LISTEN", ":8080"), "Server listen address")
	flag.StringVar(&pgDSN, "pg-dsn", getEnv("POSTGRES_DSN", "host=localhost port=5432 user=postgres password=200603 dbname=audit sslmode=disable"), "PostgreSQL DSN")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

type Server struct {
	db *sql.DB
}

func NewServer() (*Server, error) {
	db, err := sql.Open("postgres", pgDSN)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping database: %v", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	return &Server{db: db}, nil
}

func (s *Server) HandleUpload(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%s] Received upload request from %s", time.Now().Format("2006-01-02 15:04:05"), r.RemoteAddr)

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req audit.UploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("Invalid request: %v", err)
		http.Error(w, fmt.Sprintf("Invalid request: %v", err), http.StatusBadRequest)
		return
	}

	log.Printf("Upload request: MessageID=%s, SessionID=%s, Records=%d", req.MessageID, req.SessionID, len(req.Records))

	resp := audit.UploadResponse{
		MessageID: req.MessageID,
		Timestamp: time.Now(),
	}

	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	inserted := 0
	skipped := 0

	for _, record := range req.Records {
		result, err := tx.Exec(`INSERT INTO records (id, message_id, session_id, seq_num, timestamp, type, is_input, data, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (id) DO NOTHING`,
			record.ID, record.MessageID, record.SessionID, record.SeqNum,
			record.Timestamp, record.Type, record.IsInput, record.Data, record.CreatedAt)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to insert record: %v", err), http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected > 0 {
			inserted++
		} else {
			skipped++
			log.Printf("Duplicate record skipped: MessageID=%s, SeqNum=%d", record.MessageID, record.SeqNum)
		}
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, fmt.Sprintf("Failed to commit: %v", err), http.StatusInternalServerError)
		return
	}

	resp.Success = true
	log.Printf("Stored %d records, skipped %d duplicates for SessionID=%s", inserted, skipped, req.SessionID)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Database unavailable"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) HandleGetRecords(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "Missing session_id", http.StatusBadRequest)
		return
	}

	rows, err := s.db.Query(`SELECT id, message_id, session_id, seq_num, timestamp, type, is_input, data, created_at
		FROM records WHERE session_id = $1 ORDER BY seq_num ASC`, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []audit.Record
	for rows.Next() {
		var r audit.Record
		if err := rows.Scan(&r.ID, &r.MessageID, &r.SessionID, &r.SeqNum, &r.Timestamp, &r.Type, &r.IsInput, &r.Data, &r.CreatedAt); err != nil {
			http.Error(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
			return
		}
		records = append(records, r)
	}

	json.NewEncoder(w).Encode(records)
}

func (s *Server) Close() error {
	return s.db.Close()
}

func main() {
	_ = godotenv.Load()
	flag.Parse()

	if envListen := os.Getenv("SERVER_LISTEN"); envListen != "" {
		listenAddr = envListen
	}
	if envDSN := os.Getenv("POSTGRES_DSN"); envDSN != "" {
		pgDSN = envDSN
	}

	log.Printf("Connecting to PostgreSQL with DSN: %s", pgDSN)
	server, err := NewServer()
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	http.HandleFunc("/health", server.HandleHealth)
	http.HandleFunc("/api/v1/audit/upload", server.HandleUpload)
	http.HandleFunc("/api/v1/records", server.HandleGetRecords)

	go func() {
		log.Printf("Server listening on %s", listenAddr)
		if err := http.ListenAndServe(listenAddr, nil); err != nil {
			log.Fatalf("Server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	<-ctx.Done()
	log.Println("Server stopped")
}
