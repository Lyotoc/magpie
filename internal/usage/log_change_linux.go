package usage

import (
	"os"
	"syscall"
	"time"
)

// ctime changes even when a writer restores the size and modification time.
func nativeLogChangeTime(info os.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}
	}
	// Ctim.Sec and Ctim.Nsec are int32 on 32-bit linux/arm, int64 on 64-bit.
	return time.Unix(int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec))
}
