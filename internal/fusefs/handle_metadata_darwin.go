package fusefs

import (
	"syscall"
	"time"
)

func handleStatTimes(stat *syscall.Stat_t) (time.Time, time.Time, time.Time, time.Time) {
	return time.Unix(stat.Atimespec.Sec, stat.Atimespec.Nsec), time.Unix(stat.Mtimespec.Sec, stat.Mtimespec.Nsec), time.Unix(stat.Ctimespec.Sec, stat.Ctimespec.Nsec), time.Unix(stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec)
}
