package syncer

import (
	"os"
	"testing"
	"time"

	"github.com/serebro/icloudsync/internal/icloud"
)

func TestMain(m *testing.M) {
	icloud.RetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	os.Exit(m.Run())
}
