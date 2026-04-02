//go:build linux

package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

type Session struct {
	ptmx   *os.File
	cmd    *exec.Cmd
	done   chan struct{}
	output io.Reader
	pgid   int
}

func NewSession(shell string) (*Session, error) {
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			// Try common shells in order of preference
			for _, candidate := range []string{"/bin/sh", "/bin/bash", "/usr/bin/bash"} {
				if _, err := os.Stat(candidate); err == nil {
					shell = candidate
					break
				}
			}
			if shell == "" {
				shell = "/bin/sh"
			}
		}
	}

	cmd := exec.Command(shell)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}

	s := &Session{
		ptmx:   ptmx,
		cmd:    cmd,
		done:   make(chan struct{}),
		output: ptmx,
		pgid:   cmd.Process.Pid,
	}

	return s, nil
}

func NewSessionWithAttr(shell string, attr *os.ProcAttr) (*Session, error) {
	return NewSession(shell)
}

func (s *Session) Read() ([]byte, error) {
	buf := make([]byte, 65536)
	for {
		select {
		case <-s.done:
			return nil, errors.New("session closed")
		default:
		}

		n, err := s.output.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, err
			}
			select {
			case <-s.done:
				return nil, errors.New("session closed")
			default:
			}
			continue
		}

		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			return data, nil
		}
	}
}

func (s *Session) ReadWithTimeout(timeout string) ([]byte, error) {
	return s.Read()
}

func (s *Session) Write(data []byte) (int, error) {
	select {
	case <-s.done:
		return 0, errors.New("session closed")
	default:
	}
	return s.ptmx.Write(data)
}

func (s *Session) WriteSync(data []byte) (int, error) {
	return s.Write(data)
}

func (s *Session) Start() error {
	return nil
}

func (s *Session) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	close(s.done)

	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Signal(syscall.SIGTERM)
	}

	return s.ptmx.Close()
}

func (s *Session) GetWindowSize() (rows, cols uint16, err error) {
	return 40, 120, nil
}

func (s *Session) SetWindowSize(rows, cols uint16) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{
		Rows: rows,
		Cols: cols,
	})
}

func (s *Session) PID() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return -1
}

func (s *Session) IsClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) OutputCh() <-chan []byte {
	return nil
}

func (s *Session) InputCh() chan<- []byte {
	return nil
}

func (s *Session) IsPty() bool {
	return true
}

func (s *Session) Wait() error {
	return s.ptmx.Close()
}

func (s *Session) SendInterrupt() error {
	if s.IsClosed() {
		return errors.New("session closed")
	}
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Signal(syscall.SIGINT)
	}
	return nil
}

func (s *Session) KillProcessGroup() error {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Kill()
	}
	return nil
}
