//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var moveFileExW = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceFile(oldPath, newPath string) error {
	oldPtr, err := syscall.UTF16PtrFromString(oldPath)
	if err != nil {
		return err
	}
	newPtr, err := syscall.UTF16PtrFromString(newPath)
	if err != nil {
		return err
	}
	ok, _, callErr := moveFileExW.Call(
		uintptr(unsafe.Pointer(oldPtr)),
		uintptr(unsafe.Pointer(newPtr)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if ok == 0 {
		return fmt.Errorf("原子替换配置: %w", callErr)
	}
	return nil
}

// securePath 把路径收紧为只允许当前用户访问：清掉显式规则、断开继承、只留当前
// 用户 FullControl。
//
// 用 icacls 而不是 PowerShell 的 Set-Acl：Set-Acl 把安全描述符按
// AccessControlSections.All 写回，其中的 SACL 段要求 SeSecurityPrivilege，普通
// 用户没有这项特权，真机上恒被拒（真 Windows 实测：「该进程不具有执行此操作所需
// 的 SeSecurityPrivilege 特权」）。从 Get-Acl 读出的描述符改起也一样被拒——要
// SACL 的是 Set-Acl 自己，不是描述符里带没带那一段。绕开 cmdlet 直接调 .NET 的
// SetAccessControl 则在两种宿主上不是同一套 API：Windows PowerShell 5.1（.NET
// Framework）有 FileInfo 实例方法，pwsh（.NET Core）只有扩展方法，一份脚本盖不住
// 两边。icacls 只动 DACL，不碰 SACL，任何 Windows 上都在，也不需要任何特权。
//
// 三步分开调用，顺序是「清显式 → 授权 → 断继承」：断继承放最后，结果就不依赖
// icacls 内部先处理哪个开关，任何实现下都不会中途留下一个谁都进不去的空 DACL。
// 属主始终不动——文件本来就是当前用户建的，改属主只会多要一项特权；而属主无论
// DACL 写成什么都保有 WRITE_DAC，三步之间不会把自己锁在外面。重复执行结果相同。
func securePath(path string, directory bool) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	tool, err := icaclsPath()
	if err != nil {
		return err
	}
	// 简单权限 F 不加括号、继承标志写在它前面的括号里，是 icacls 文档给的写法。
	grant := "*" + sid + ":F"
	if directory {
		grant = "*" + sid + ":(OI)(CI)F"
	}
	target := filepath.Clean(path)
	for _, args := range [][]string{
		{target, "/reset"},
		{target, "/grant:r", grant},
		{target, "/inheritance:r"},
	} {
		cmd := exec.Command(tool, args...)
		cmd.Env = appendWithout(os.Environ(), "LLMGATE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN")
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("收紧 Windows 配置 ACL: %s", clipped(b))
		}
	}
	return nil
}

// currentUserSID 取当前用户的 SID。os/user 在 Windows 上把 SID 放进 Uid，且不
// 依赖 cgo。用 SID 而不是用户名：用户名要再经一次名称解析，跨域、改名与本地化的
// 内建名都可能对不上，SID 是逐字可用的主体。
func currentUserSID() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("读取当前 Windows 用户: %w", err)
	}
	if !strings.HasPrefix(u.Uid, "S-1-") {
		return "", fmt.Errorf("当前 Windows 用户没有可用的 SID：%q", u.Uid)
	}
	return u.Uid, nil
}

// icaclsPath 先认 %SystemRoot%\System32 下的绝对路径，取不到再退回 PATH 查找：
// 这是一处收紧权限的动作，不该被 PATH 上先出现的同名程序顶替。
func icaclsPath() (string, error) {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "icacls.exe")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	p, err := exec.LookPath("icacls.exe")
	if err != nil {
		return "", errors.New("找不到 icacls.exe，无法收紧配置 ACL")
	}
	return p, nil
}
