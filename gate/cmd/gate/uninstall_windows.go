//go:build windows

package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

//go:embed uninstall_finish.ps1
var uninstallFinishScript []byte

func isReparse(st os.FileInfo) bool {
	data, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
func isTerminalFile(f *os.File) bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Handle(f.Fd()), &mode) == nil
}
func lockFile(f *os.File, shared bool) error {
	flags := uintptr(1)
	if !shared {
		flags |= 2
	}
	var overlap syscall.Overlapped
	ok, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx").Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&overlap)))
	if ok == 0 {
		return err
	}
	return nil
}

func systemPowerShell() (string, error) {
	var buf [32768]uint16
	n, _, err := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || n >= uintptr(len(buf)) {
		return "", fmt.Errorf("定位系统目录：%w", err)
	}
	path := filepath.Join(syscall.UTF16ToString(buf[:n]), "WindowsPowerShell", "v1.0", "powershell.exe")
	if _, err := os.Stat(path); err != nil {
		return "", errors.New("找不到系统 Windows PowerShell，无法准备自卸载")
	}
	return path, nil
}

func systemPS(script string, in []byte) ([]byte, error) {
	host, err := systemPowerShell()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(host, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = appendWithout(os.Environ(), "LLMGATE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	b, err := cmd.Output()
	if err != nil {
		return nil, errors.New("系统 PowerShell 操作失败")
	}
	return b, nil
}
func readUserPath() (string, error) {
	b, err := systemPS(`$ErrorActionPreference='Stop'; $k=[Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment'); $v=if($k){$k.GetValue('Path','',[Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)}else{''}; ConvertTo-Json -InputObject ([string]$v) -Compress`, nil)
	if err != nil {
		return "", err
	}
	var value string
	err = json.Unmarshal(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), &value)
	return value, err
}
func writeUserPath(value string) error {
	b, _ := json.Marshal(value)
	_, err := systemPS(`$ErrorActionPreference='Stop'; $v=ConvertFrom-Json ([Console]::In.ReadToEnd()); $k=[Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment'); $kind=[Microsoft.Win32.RegistryValueKind]::ExpandString; if($k.GetValueNames() -contains 'Path'){$kind=$k.GetValueKind('Path')}; $k.SetValue('Path',[string]$v,$kind); $k.Close()`, b)
	return err
}

type uninstallJob struct {
	CleanupDirectory string `json:"cleanup_directory,omitempty"`
	KeepResult       bool   `json:"keep_result,omitempty"`
	Schema           int    `json:"schema"`
	Target           string `json:"target"`
	Identity         string `json:"identity"`
	Hash             string `json:"sha256"`
	PID              int    `json:"pid"`
	Started          string `json:"started"`
}

func windowsFileIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%08x-%08x-%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}

func prepareSelfRemoval(c *fileChange, out, errOut io.Writer) (func() error, func(), error) {
	noop := func() {}
	if c == nil {
		return func() error { return nil }, noop, nil
	}
	if err := c.check(); err != nil {
		return nil, noop, err
	}
	host, err := systemPowerShell()
	if err != nil {
		return nil, noop, err
	}
	id, err := windowsFileIdentity(c.path)
	if err != nil {
		return nil, noop, err
	}
	var created, exited, kernel, usr syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, noop, err
	}
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &usr); err != nil {
		return nil, noop, err
	}
	started := fmt.Sprint(uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime))
	dir, err := os.MkdirTemp("", "gate-uninstall-")
	if err != nil {
		return nil, noop, err
	}
	if err := securePath(dir, true); err != nil {
		os.Remove(dir)
		return nil, noop, err
	}
	jobPath, scriptPath := filepath.Join(dir, "job.json"), filepath.Join(dir, "finish.ps1")
	committed := false
	startedHelper := false
	cancel := func() {
		if !committed {
			if !startedHelper {
				for _, name := range []string{"job.json", "finish.ps1"} {
					_ = os.Remove(filepath.Join(dir, name))
				}
				_ = os.Remove(dir)
				return
			}
			_ = os.WriteFile(filepath.Join(dir, "cancel"), []byte("cancel"), 0600)
		}
	}
	job := uninstallJob{Schema: 1, Target: c.path, Identity: id, Hash: c.hash, PID: os.Getpid(), Started: started, KeepResult: !isTerminal(out)}
	if c.emptyParent {
		job.CleanupDirectory = filepath.Dir(c.path)
	}
	b, _ := json.Marshal(job)
	if err := atomicWrite(jobPath, b, 0600); err != nil {
		return nil, cancel, err
	}
	// A UTF-8 BOM is required by Windows PowerShell 5.1 for Chinese diagnostics.
	if err := atomicWrite(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, uninstallFinishScript...), 0600); err != nil {
		return nil, cancel, err
	}
	cmd := exec.Command(host, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath, "-Job", jobPath)
	// Pass terminal handles directly; exec's copy goroutines would die with gate.
	if f, ok := out.(*os.File); ok {
		cmd.Stdout = f
	}
	if f, ok := errOut.(*os.File); ok {
		cmd.Stderr = f
	}
	cmd.Env = appendWithout(os.Environ(), "LLMGATE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	if err := cmd.Start(); err != nil {
		return nil, cancel, errors.New("无法启动系统 PowerShell 收尾进程")
	}
	startedHelper = true
	go cmd.Wait()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			return func() error {
				if err := c.check(); err != nil {
					return err
				}
				if err := atomicWrite(filepath.Join(dir, "commit"), []byte("commit"), 0600); err != nil {
					return err
				}
				committed = true
				fmt.Fprintln(out, "Windows 收尾结果："+filepath.Join(dir, "result.txt"))
				return nil
			}, cancel, nil
		}
		if _, err := os.Stat(filepath.Join(dir, "result.txt")); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, cancel, fmt.Errorf("Windows 收尾预检未就绪；gate 已保留。结果：%s", filepath.Join(dir, "result.txt"))
}
func printSelfRemovalResult(out io.Writer) {
	fmt.Fprintln(out, "配置清理完成，程序删除待收尾；请以收尾结果和 gate.exe 已不存在为准。请重新打开终端。")
}

func checkProgramUse(p *uninstallPlan) error {
	for _, c := range p.changes {
		if c.label != "删除受管程序" || c.link != "" {
			continue
		}
		path, err := syscall.UTF16PtrFromString(c.path)
		if err != nil {
			return err
		}
		h, err := syscall.CreateFile(path, 0x00010000, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			return fmt.Errorf("受管文件被占用或没有删除权限，请关闭工具后重试：%s", c.path)
		}
		syscall.CloseHandle(h)
	}
	return nil
}
