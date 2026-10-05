package fusefs

import (
	"syscall"
	"time"
)

func handleStatTimes(stat *syscall.Stat_t) (time.Time, time.Time, time.Time, time.Time) {
	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec), time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec), time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec), time.Time{}
}
