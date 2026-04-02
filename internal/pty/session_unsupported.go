//go:build !linux

package pty

import (
	"errors"
	"fmt"
	"os"
)

type Session struct {
	done chan struct{}
}

func NewSession(shell string) (*Session, error) {
	return nil, errors.New("PTY is only supported on Linux")
}

func (s *Session) Read() ([]byte, error) {
	return nil, errors.New("session closed")
}

func (s *Session) ReadWithTimeout(timeout string) ([]byte, error) {
	return nil, errors.New("session closed")
}

func (s *Session) Write(data []byte) (int, error) {
	return 0, errors.New("session closed")
}

func (s *Session) WriteSync(data []byte) (int, error) {
	return 0, errors.New("session closed")
}

func (s *Session) Start() error {
	return errors.New("PTY is only supported on Linux")
}

func (s *Session) Close() error {
	return nil
}

func (s *Session) GetWindowSize() (uint16, uint16, error) {
	return 40, 120, nil
}

func (s *Session) SetWindowSize(rows, cols uint16) error {
	return errors.New("PTY is only supported on Linux")
}

func (s *Session) PID() int {
	return -1
}

func (s *Session) IsClosed() bool {
	return true
}

func (s *Session) OutputCh() <-chan []byte {
	return nil
}

func (s *Session) InputCh() chan<- []byte {
	return nil
}

func (s *Session) IsPty() bool {
	return false
}

func (s *Session) Wait() error {
	return fmt.Errorf("PTY is only supported on Linux")
}

func (s *Session) SendInterrupt() error {
	return fmt.Errorf("PTY is only supported on Linux")
}

func (s *Session) KillProcessGroup() error {
	return fmt.Errorf("PTY is only supported on Linux")
}

func NewSessionWithAttr(shell string, attr *os.ProcAttr) (*Session, error) {
	return nil, errors.New("PTY is only supported on Linux")
}
