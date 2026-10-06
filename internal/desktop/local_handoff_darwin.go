//go:build darwin

package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func handoffBirthTime(stat *unix.Stat_t) (int64, int64) {
	return stat.Btim.Sec, stat.Btim.Nsec
}

func handoffRenameExclusive(source *os.File, name string, destination *os.File, target string) error {
	return unix.RenameatxNp(int(source.Fd()), name, int(destination.Fd()), target, unix.RENAME_EXCL)
}

func handoffDirectoryNotInUse(ctx context.Context, path string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Only process IDs are requested; no filenames, arguments or configuration
	// are captured. The quarantined random path is not in the visible catalogue.
	command := exec.CommandContext(checkCtx, "/usr/sbin/lsof", "-nP", "-F", "p", "+D", path)
	output, err := command.Output()
	if checkCtx.Err() != nil {
		return errors.New("could not verify that the checkout is closed; its files were retained")
	}
	if len(output) != 0 {
		return errors.New("the local checkout is open in another application; close it before freeing space")
	}
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && len(exit.Stderr) == 0) {
		return errors.New("could not verify that the checkout is closed; its files were retained")
	}
	return nil
}
