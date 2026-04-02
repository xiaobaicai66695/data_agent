package idgen

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	epoch           = int64(1609459200000)
	timestampBits   = uint(41)
	datacenterBits  = uint(5)
	workerIDBits    = uint(5)
	sequenceBits    = uint(12)

	maxTimestamp = int64(-1) ^ (int64(-1) << timestampBits)
	maxWorkerID  = int64(-1) ^ (int64(-1) << workerIDBits)
	maxSequence  = int64(-1) ^ (int64(-1) << sequenceBits)
)

var (
	mu         sync.Mutex
	workerID   int64 = 1
	sequence   int64 = 0
	lastTime   int64 = -1
)

func init() {
}

func SetWorkerID(id int64) error {
	if id < 0 || id > maxWorkerID {
		return errors.New("worker ID out of range")
	}
	workerID = id
	return nil
}

func GenerateMessageID() int64 {
	mu.Lock()
	defer mu.Unlock()

	now := time.Now().UnixNano()/1000000 + epoch

	if now == lastTime {
		sequence = (sequence + 1) & maxSequence
		if sequence == 0 {
			for now <= lastTime {
				now = time.Now().UnixNano()/1000000 + epoch
			}
		}
	} else {
		sequence = 0
	}

	lastTime = now

	id := (now << (datacenterBits + workerIDBits + sequenceBits)) |
		(workerID << (datacenterBits + sequenceBits)) |
		sequence

	return id
}

func GenerateSessionID() string {
	return fmt.Sprintf("%d", GenerateMessageID())
}
