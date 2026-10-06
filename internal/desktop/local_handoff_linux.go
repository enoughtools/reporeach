//go:build linux

package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func handoffBirthTime(*unix.Stat_t) (int64, int64) { return 0, 0 }

func handoffRenameExclusive(source *os.File, name string, destination *os.File, target string) error {
	return unix.Renameat2(int(source.Fd()), name, int(destination.Fd()), target, unix.RENAME_NOREPLACE)
}

func handoffDirectoryNotInUse(ctx context.Context, path string) error {
	tool, err := exec.LookPath("lsof")
	if err != nil {
		return errors.New("checking open checkout files requires lsof; files were retained")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(checkCtx, tool, "-nP", "-F", "p", "+D", path)
	output, err := command.Output()
	if checkCtx.Err() != nil || len(output) != 0 {
		return errors.New("the local checkout may be open in another application; its files were retained")
	}
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1 && len(exit.Stderr) == 0) {
		return errors.New("could not verify that the checkout is closed; its files were retained")
	}
	return nil
}
