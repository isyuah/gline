//go:build windows

package source

import (
	"fmt"
	"os"
	"syscall"
)

func persistentFileIdentity(_ string, file *os.File, _ os.FileInfo) (string, error) {
	var data syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(file.Fd()), &data); err != nil {
		return "", fmt.Errorf("read Windows file identity: %w", err)
	}
	index := uint64(data.FileIndexHigh)<<32 | uint64(data.FileIndexLow)
	return fmt.Sprintf("windows:%08x:%016x", data.VolumeSerialNumber, index), nil
}
