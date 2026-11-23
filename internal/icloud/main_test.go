package icloud

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	os.Exit(m.Run())
}
