package devhost

// 一台主机上装哪个守护进程由它的类型决定：工作节点装 devd（llmgate-devd），模型服务节点装
// modeld（llmgate-modeld）。两者的安装 / 卸载 / 检查 / 透传流程完全相同，只有二进制、unit、
// 配置目录与 socket 路径不同——这份规格把差异收在一处，Manager 的其余代码按规格走。

import (
	"github.com/llm-net/llm-gate/firmware/internal/devd"
	"github.com/llm-net/llm-gate/firmware/internal/modeld"
	"github.com/llm-net/llm-gate/firmware/internal/store"
)

// daemonSpec 是一种守护进程在主机上的布局。
type daemonSpec struct {
	// Name 是短名（devd / modeld），也是界面上的叫法与制品前缀的一部分。
	Name string
	// BinName 是二进制文件名（llmgate-devd / llmgate-modeld），也是内嵌制品 assets/<BinName>-linux-<arch>.gz 的前缀。
	BinName   string
	BinPath   string
	UnitName  string
	ConfigDir string
	Socket    string
}

var (
	devdSpec = daemonSpec{Name: "devd", BinName: "llmgate-devd", BinPath: devd.DefaultBinPath, UnitName: devd.UnitName,
		ConfigDir: devd.DefaultConfigDir, Socket: devd.DefaultSocket}
	modeldSpec = daemonSpec{Name: "modeld", BinName: "llmgate-modeld", BinPath: modeld.DefaultBinPath, UnitName: modeld.UnitName,
		ConfigDir: modeld.DefaultConfigDir, Socket: modeld.DefaultSocket}
)

// specFor 是这台主机该装的守护进程；受控纳管没有守护进程时也给 devd 的规格（只用于文案）。
func specFor(host *store.AgentHost) daemonSpec {
	if host != nil && host.IsModelService() {
		return modeldSpec
	}
	return devdSpec
}

// DaemonSpecs 列出全部守护进程的短名与二进制名（Makefile 与测试对账用）。
func DaemonSpecs() [][2]string {
	return [][2]string{{devdSpec.Name, devdSpec.BinName}, {modeldSpec.Name, modeldSpec.BinName}}
}
