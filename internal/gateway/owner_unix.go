//go:build unix

package gateway

import (
	"os"
	"syscall"
)

// matchOwner gives file the owner of dir, when this process may do that. It's
// for files made by root on the gateway's behalf; anything else is left alone.
func matchOwner(file, dir string) {
	info, err := os.Stat(dir)
	if err != nil {
		return
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		_ = os.Chown(file, int(stat.Uid), int(stat.Gid))
	}
}
