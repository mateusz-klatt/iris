//go:build linux

package proc

import (
	"context"
	"os"
	"os/exec"
	"strconv"
)

type exeHandle struct {
	file *os.File
}

// Close releases any open executable descriptor held by e.
func (e *ProcessExecutable) Close() error {
	if e == nil || e.file == nil {
		return nil
	}
	f := e.file
	e.file = nil
	return f.Close()
}

// Command prepares cmd to execute e with args. When e holds an open descriptor to a deleted
// running image, cmd executes /proc/self/fd/3 with that descriptor in ExtraFiles so the old
// image runs without consulting the filesystem path or racing PID reuse. Close e after
// cmd.Start returns (or if Start is not called).
func (e *ProcessExecutable) Command(ctx context.Context, args ...string) *exec.Cmd {
	if e.file != nil {
		cmd := exec.CommandContext(ctx, "/proc/self/fd/3", args...)
		cmd.ExtraFiles = []*os.File{e.file}
		return cmd
	}
	return exec.CommandContext(ctx, e.Path, args...)
}

func openDeletedExeOS(pid int, startMS int64, exe string, get func(int) (Snapshot, bool)) (*ProcessExecutable, error) {
	return openDeletedExeFrom(pid, startMS, exe, get, os.Open)
}

func openDeletedExeFrom(
	pid int,
	startMS int64,
	exe string,
	get func(int) (Snapshot, bool),
	open func(string) (*os.File, error),
) (*ProcessExecutable, error) {
	f, err := open("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return nil, deletedExeErr(pid, startMS, get)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, deletedExeErr(pid, startMS, get)
	}
	after, ok := get(pid)
	if !ok || after.StartMS != startMS {
		_ = f.Close()
		return nil, ErrNotFound
	}
	return &ProcessExecutable{Path: exe, Deleted: true, exeHandle: exeHandle{file: f}}, nil
}

func deletedExeErr(pid int, startMS int64, get func(int) (Snapshot, bool)) error {
	after, ok := get(pid)
	if !ok || after.StartMS != startMS {
		return ErrNotFound
	}
	return ErrExecutableDeleted
}
