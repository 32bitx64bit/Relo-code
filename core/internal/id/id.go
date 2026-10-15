package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

var fallbackCounter atomic.Uint64

func New(prefix string) string {
	return newWithRandom(prefix, rand.Read)
}

func newWithRandom(prefix string, read func([]byte) (int, error)) string {
	var b [10]byte
	if _, err := read(b[:]); err != nil {
		return fmt.Sprintf("%s_%x_%x", prefix, time.Now().UnixNano(), fallbackCounter.Add(1))
	}
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(b[:]))
}
