//go:build !windows

package persistence

import "golang.org/x/sys/unix"

func legacyAvailableSpace(path string) (uint64, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return fs.Bavail * uint64(fs.Bsize), nil
}
