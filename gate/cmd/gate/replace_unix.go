//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

func replaceFile(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(newPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func securePath(string, bool) error { return nil }
