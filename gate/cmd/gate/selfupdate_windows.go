//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const selfUpdateJobSchema = 1

type selfUpdateJob struct {
	ConfigRoot      string `json:"config_root"`
	TargetIdentity  string `json:"target_identity"`
	SchemaVersion   int    `json:"schema_version"`
	Target          string `json:"target"`
	Payload         string `json:"payload"`
	Backup          string `json:"backup"`
	Helper          string `json:"helper"`
	ExpectedVersion string `json:"expected_version"`
	PayloadSHA256   string `json:"payload_sha256"`
}

func runSelfUpdateInternal(args []string) (bool, int) {
	if len(args) == 2 && args[0] == "__soc_self_update_apply" {
		if err := runSelfUpdateApply(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "gate:", err)
			return true, 1
		}
		return true, 0
	}
	if len(args) == 2 && args[0] == "__soc_self_update_cleanup" {
		runSelfUpdateCleanup(args[1])
		return true, 0
	}
	return false, 0
}

func applySelfUpdate(target, staged, expectedVersion, configRoot string) (bool, error) {
	identity, err := windowsFileIdentity(target)
	if err != nil {
		return false, err
	}
	dir := filepath.Dir(target)
	backup, err := copyFileToTemp(target, dir, ".gate-update-old-*.exe")
	if err != nil {
		return false, fmt.Errorf("备份当前 gate: %w", err)
	}
	helper, err := copyFileToTemp(staged, dir, ".gate-update-helper-*.exe")
	if err != nil {
		os.Remove(backup)
		return false, fmt.Errorf("准备 Windows 升级进程: %w", err)
	}
	digest, err := fileSHA256(staged)
	if err != nil {
		os.Remove(backup)
		os.Remove(helper)
		return false, err
	}
	job := selfUpdateJob{
		ConfigRoot: configRoot, TargetIdentity: identity, SchemaVersion: selfUpdateJobSchema, Target: target, Payload: staged, Backup: backup,
		Helper: helper, ExpectedVersion: expectedVersion, PayloadSHA256: digest,
	}
	jobFile, err := os.CreateTemp(dir, ".gate-update-job-*.json")
	if err != nil {
		os.Remove(backup)
		os.Remove(helper)
		return false, err
	}
	jobPath := jobFile.Name()
	cleanup := true
	defer func() {
		_ = jobFile.Close()
		if cleanup {
			_ = os.Remove(jobPath)
			_ = os.Remove(backup)
			_ = os.Remove(helper)
		}
	}()
	if err := jobFile.Chmod(0o600); err != nil {
		return false, err
	}
	if err := json.NewEncoder(jobFile).Encode(job); err != nil {
		return false, err
	}
	if err := jobFile.Sync(); err != nil {
		return false, err
	}
	if err := jobFile.Close(); err != nil {
		return false, err
	}
	cmd := exec.Command(helper, "__soc_self_update_apply", jobPath)
	cmd.Env = installerEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("启动 Windows 升级进程: %w", err)
	}
	_ = cmd.Process.Release()
	cleanup = false
	return true, nil
}

func runSelfUpdateApply(jobPath string) error {
	job, err := readAndValidateSelfUpdateJob(jobPath)
	if err != nil {
		return err
	}
	defer os.Remove(jobPath)
	var lease *operationLease
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		lease, err = acquireOperation(job.ConfigRoot, false)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	defer lease.close()
	identity, err := windowsFileIdentity(job.Target)
	if err != nil || identity != job.TargetIdentity {
		return errors.New("升级目标已被替换或卸载，已停止收尾")
	}
	if digest, err := fileSHA256(job.Payload); err != nil || digest != job.PayloadSHA256 {
		return errors.New("Windows 升级载荷校验失败")
	}
	if err := retryWindowsReplace(job.Payload, job.Target); err != nil {
		return err
	}
	if err := verifyGateVersion(job.Target, job.ExpectedVersion); err != nil {
		if rollbackErr := retryWindowsReplace(job.Backup, job.Target); rollbackErr != nil {
			return fmt.Errorf("Windows 升级后校验失败（%v），回退也失败: %w", err, rollbackErr)
		}
		return fmt.Errorf("Windows 升级后校验失败，已恢复原程序: %w", err)
	}
	if err := (&app{root: job.ConfigRoot}).recordExecutable(job.Target); err != nil {
		return err
	}
	_ = os.Remove(job.Backup)
	cleanup := exec.Command(job.Target, "__soc_self_update_cleanup", job.Helper)
	cleanup.Env = installerEnv()
	cleanup.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200, HideWindow: true}
	if err := cleanup.Start(); err != nil {
		return err
	}
	_ = cleanup.Process.Release()
	return nil
}

func readAndValidateSelfUpdateJob(path string) (selfUpdateJob, error) {
	var job selfUpdateJob
	if !strings.HasPrefix(filepath.Base(path), ".gate-update-job-") || filepath.Ext(path) != ".json" {
		return job, errors.New("Windows 升级任务路径非法")
	}
	file, err := os.Open(path)
	if err != nil {
		return job, err
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&job); err != nil {
		return job, err
	}
	if err := ensureJSONEOF(dec); err != nil {
		return job, err
	}
	if !filepath.IsAbs(job.ConfigRoot) || job.TargetIdentity == "" {
		return job, errors.New("Windows 升级任务缺少安装归属")
	}
	if job.SchemaVersion != selfUpdateJobSchema || !gateReleaseVersionRE.MatchString(job.ExpectedVersion) || !gateReleaseDigestRE.MatchString(job.PayloadSHA256) {
		return job, errors.New("Windows 升级任务内容非法")
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return job, err
	}
	for _, item := range []struct {
		path, prefix string
	}{
		{job.Payload, ".gate-update-new-"}, {job.Backup, ".gate-update-old-"}, {job.Helper, ".gate-update-helper-"},
	} {
		abs, err := filepath.Abs(item.path)
		if err != nil || !strings.EqualFold(filepath.Dir(abs), dir) || !strings.HasPrefix(filepath.Base(abs), item.prefix) {
			return job, errors.New("Windows 升级任务文件范围非法")
		}
	}
	target, err := filepath.Abs(job.Target)
	if err != nil || !strings.EqualFold(filepath.Dir(target), dir) {
		return job, errors.New("Windows 升级目标范围非法")
	}
	current, err := os.Executable()
	if err != nil {
		return job, err
	}
	current, _ = filepath.Abs(current)
	if !strings.EqualFold(current, job.Helper) {
		return job, errors.New("Windows 升级任务不是由指定辅助程序执行")
	}
	return job, nil
}

func retryWindowsReplace(oldPath, newPath string) error {
	deadline := time.Now().Add(60 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if err := replaceFile(oldPath, newPath); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("等待运行中的 gate 退出后替换: %w", last)
}

func runSelfUpdateCleanup(helper string) {
	if !strings.HasPrefix(filepath.Base(helper), ".gate-update-helper-") {
		return
	}
	current, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Dir(current), filepath.Dir(helper)) {
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := os.Remove(helper); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, selfUpdateBinaryMax+1))
	if err != nil {
		return "", err
	}
	if n <= 0 || n > selfUpdateBinaryMax {
		return "", errors.New("Windows 升级程序大小非法")
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}
