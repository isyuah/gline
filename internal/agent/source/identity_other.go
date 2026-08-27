//go:build !windows && !unix

package source

import (
	"fmt"
	"os"
)

func persistentFileIdentity(path string, _ *os.File, info os.FileInfo) (string, error) {
	return fmt.Sprintf("portable:%s:%d", path, info.ModTime().UnixNano()), nil
}
