//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func runSelfUpdateInternal([]string) (bool, int) { return false, 0 }

func applySelfUpdate(target, staged, expectedVersion, configRoot string) (bool, error) {
	dir := filepath.Dir(target)
	backup, err := copyFileToTemp(target, dir, ".gate-update-old-*")
	if err != nil {
		return false, fmt.Errorf("备份当前 gate: %w", err)
	}
	defer os.Remove(backup)
	if err := replaceFile(staged, target); err != nil {
		return false, fmt.Errorf("替换当前 gate: %w", err)
	}
	if err := verifyGateVersion(target, expectedVersion); err != nil {
		if rollbackErr := replaceFile(backup, target); rollbackErr != nil {
			return false, fmt.Errorf("升级后校验失败（%v），回退也失败: %w", err, rollbackErr)
		}
		return false, fmt.Errorf("升级后校验失败，已恢复原程序: %w", err)
	}
	return false, nil
}
