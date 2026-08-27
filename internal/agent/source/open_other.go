//go:build !windows

package source

import "os"

func openReadFile(path string) (*os.File, error) { return os.Open(path) }
