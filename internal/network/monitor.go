package network

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"
)

type Monitor struct {
	serverURL           string
	isOnline            bool
	mu                  sync.RWMutex
	stopCh              chan struct{}
	wg                  sync.WaitGroup
	listeners           []chan bool
	listenerMu          sync.RWMutex
	checkInterval       time.Duration
	httpClient          *http.Client
	lastCheckTime       time.Time
	consecutiveFails    int
	maxConsecutiveFails int
}

type NetworkCallback interface {
	OnNetworkStatusChanged(online bool)
}

type ReporterCallback interface {
	SetServerAvailable(available bool)
}

func NewMonitor(serverURL string) *Monitor {
	return &Monitor{
		serverURL:           serverURL,
		isOnline:            false,
		stopCh:              make(chan struct{}),
		listeners:           make([]chan bool, 0),
		checkInterval:       3 * time.Second,
		httpClient:          &http.Client{Timeout: 5 * time.Second},
		consecutiveFails:    0,
		maxConsecutiveFails: 3,
	}
}

func (m *Monitor) Start() {
	m.wg.Add(1)
	go m.checkLoop()
}

func (m *Monitor) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

func (m *Monitor) checkLoop() {
	defer m.wg.Done()

	m.checkAndNotify()

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.checkAndNotify()
		}
	}
}

func (m *Monitor) checkAndNotify() {
	online := m.checkConnectivity()

	m.mu.Lock()
	wasOnline := m.isOnline
	m.isOnline = online

	if online {
		m.consecutiveFails = 0
	} else {
		m.consecutiveFails++
	}

	changed := online != wasOnline
	m.mu.Unlock()

	if changed {
		log.Printf("[Network] Server status changed: online=%v (consecutiveFails=%d)", online, m.consecutiveFails)
		m.notifyListeners(online)
	}
}

func (m *Monitor) checkConnectivity() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, m.serverURL+"/health", nil)
	if err != nil {
		return false
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

func (m *Monitor) IsOnline() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isOnline
}

func (m *Monitor) Subscribe() chan bool {
	m.listenerMu.Lock()
	defer m.listenerMu.Unlock()

	ch := make(chan bool, 1)
	m.listeners = append(m.listeners, ch)
	return ch
}

func (m *Monitor) Unsubscribe(ch chan bool) {
	m.listenerMu.Lock()
	defer m.listenerMu.Unlock()

	for i, listener := range m.listeners {
		if listener == ch {
			m.listeners = append(m.listeners[:i], m.listeners[i+1:]...)
			close(ch)
			return
		}
	}
}

func (m *Monitor) notifyListeners(online bool) {
	m.listenerMu.RLock()
	defer m.listenerMu.RUnlock()

	for _, ch := range m.listeners {
		select {
		case ch <- online:
		default:
		}
	}
}

func (m *Monitor) WaitForOnline(ctx context.Context) error {
	if m.IsOnline() {
		return nil
	}

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case online := <-ch:
			if online {
				return nil
			}
		}
	}
}

func (m *Monitor) GetConsecutiveFails() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.consecutiveFails
}

func (m *Monitor) SetCheckInterval(interval time.Duration) {
	m.checkInterval = interval
}
