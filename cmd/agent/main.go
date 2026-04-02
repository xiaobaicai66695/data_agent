package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/audit-agent/internal/network"
	"github.com/audit-agent/internal/pty"
	"github.com/audit-agent/internal/recorder"
	"github.com/audit-agent/internal/reporter"
	"github.com/audit-agent/internal/storage"
	"github.com/audit-agent/pkg/audit"
	"github.com/audit-agent/pkg/idgen"
	"github.com/joho/godotenv"
)

var (
	serverURL      string
	dbPath         string
	shell          string
	countThreshold int
	timeThreshold  time.Duration
)

func init() {
	flag.StringVar(&serverURL, "server", getEnv("AUDIT_SERVER_URL", "http://localhost:8080"), "Audit server URL")
	flag.StringVar(&dbPath, "db", getEnv("AUDIT_DB_PATH", "./data/audit-agent.db"), "SQLite database path")
	flag.StringVar(&shell, "shell", getEnv("AUDIT_SHELL", ""), "Shell to use (empty for auto-detect)")
    // 整数类型 - 用 getEnvInt
    flag.IntVar(&countThreshold, "count-threshold", 
        getEnvInt("AUDIT_COUNT_THRESHOLD", 20), 
        "Batch size threshold for triggering upload")
    
    // 时间类型 - 用 getEnvDuration
    flag.DurationVar(&timeThreshold, "time-threshold", 
        getEnvDuration("AUDIT_TIME_THRESHOLD", 5*time.Second), 
        "Time threshold for triggering upload")
}

func getEnvStr(key, defaultValue string) string {
    if value := os.Getenv(key); value != "" {
        return value
    }
    return defaultValue
}

// 整数类型
func getEnvInt(key string, defaultValue int) int {
    if value := os.Getenv(key); value != "" {
        if i, err := strconv.Atoi(value); err == nil {
            return i
        }
    }
    return defaultValue
}

// 时间类型
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
    if value := os.Getenv(key); value != "" {
        if d, err := time.ParseDuration(value); err == nil {
            return d
        }
    }
    return defaultValue
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func main() {
	// Load .env file first so env vars are available for flag defaults
	_ = godotenv.Load()

	flag.Parse()


	// Ensure data directory exists
	dataDir := filepath.Dir(dbPath)
	if dataDir != "" && dataDir != "." {
		if err := os.MkdirAll(dataDir, 0755); err != nil {
			log.Printf("Failed to create data directory: %v", err)
		}
	}

	// Setup logging
	logDir := filepath.Dir(dbPath)
	if logDir == "" {
		logDir = "."
	}
	logFile := os.Getenv("AUDIT_LOG_FILE")
	if logFile == "" {
		logFile = filepath.Join(logDir, "agent.log")
	}

	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err == nil {
		defer f.Close()
		log.SetOutput(f)
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	}

	log.Printf("[Agent] Starting with config: server=%s, db=%s", serverURL, dbPath)

	store, err := storage.NewSQLiteStore(dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	sessionID := idgen.GenerateSessionID()

	session, err := pty.NewSession(shell)
	if err != nil {
		log.Fatalf("Failed to start PTY session: %v", err)
	}

	hostname, _ := os.Hostname()
	sessionInfo := &audit.SessionInfo{
		ID:        sessionID,
		Shell:     shell,
		StartTime: time.Now(),
		Rows:      40,
		Cols:      120,
		Hostname:  hostname,
	}
	store.SaveSession(sessionInfo)

	rec := recorder.NewRecorder(session, store, sessionID)
	if err := rec.Start(); err != nil {
		log.Fatalf("Failed to start recorder: %v", err)
	}

	monitor := network.NewMonitor(serverURL)
	rep := reporter.NewReporter(store, serverURL)
	rep.SetThresholds(countThreshold, timeThreshold)
	rep.Start()
	defer rep.Stop()

	onlineCh := monitor.Subscribe()
	go func() {
		for isOnline := range onlineCh {
			rep.SetServerAvailable(isOnline)
		}
	}()

	monitor.Start()
	defer monitor.Stop()

	go func() {
		for record := range rec.GetUploadChannel() {
			rep.SubmitRecord(record)
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if monitor.IsOnline() {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					rep.UploadPendingRecords(ctx)
					cancel()
				}
			}
		}
	}()

	var wg sync.WaitGroup
	stopped := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stopped)
		runOutputLoop(rec)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		runInteractiveLoop(rec)
	}()

	// Signal handling:
	// - SIGINT (Ctrl+C): Force exit entire program
	// - SIGQUIT (Ctrl+\): Force exit entire program
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	log.Println("[Agent] Started. Ctrl+C or Ctrl+\\ to exit")

	// Block on signal
	sig := <-sigCh
	log.Printf("[Agent] Received %v, shutting down...", sig)

	rec.Stop()
	session.KillProcessGroup()
	session.Close()
	os.Exit(0)
}

func runInteractiveLoop(rec *recorder.Recorder) error {
	log.Println("[Agent] Starting interactive input loop...")

	buf := make([]byte, 4096)

	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			if err == io.EOF {
				log.Println("[Agent] stdin EOF, exiting interactive loop")
				return io.EOF
			}
			log.Printf("[Agent] stdin read error: %v", err)
			return err
		}

		if n == 0 {
			continue
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		// Check for Ctrl+C (0x03)
		for i := 0; i < n; i++ {
			if data[i] == 0x03 {
				// Ctrl+C detected - will be handled by signal
				return io.EOF
			}
		}

		written, err := rec.Write(data)
		if err != nil {
			log.Printf("[Agent] Failed to write to PTY: %v", err)
			return err
		}

		if written != n {
			log.Printf("[Agent] Partial write: %d/%d bytes", written, n)
		}
	}
}

func runOutputLoop(rec *recorder.Recorder) {
	log.Println("[Agent] Starting PTY output echo loop...")

	for {
		select {
		case <-rec.GetStoppedChannel():
			return
		case data, ok := <-rec.GetOutputChannel():
			if !ok {
				return
			}
			// Write to stdout, let terminal handle ANSI escapes
			if _, err := os.Stdout.Write(data); err != nil {
				log.Printf("[Agent] Failed to write to stdout: %v", err)
				return
			}
		}
	}
}
