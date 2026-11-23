//go:build !windows

package main

import (
	"errors"
	"os"
)

func secureSessionFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("session file must be a private regular file (chmod 600)")
	}
	return nil
}
