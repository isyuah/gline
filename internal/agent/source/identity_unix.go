//go:build unix

package source

import (
	"fmt"
	"os"
	"syscall"
)

func persistentFileIdentity(_ string, _ *os.File, info os.FileInfo) (string, error) {
	data, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("unsupported Unix file identity metadata %T", info.Sys())
	}
	return fmt.Sprintf("unix:%d:%d", uint64(data.Dev), uint64(data.Ino)), nil
}
