// Package boardinfo 采集板卡硬件诊断信息，并为官网固件索引提供本机型号。
//
// 来源随型号档案不同，见 [profile]。有板卡 EEPROM 的量产板是三个来源，缺一不可：
//
//   - 板卡 EEPROM（/sys/bus/i2c/devices/0-0050/eeprom，**root only**）：
//     厂商、SKU、PCB 版本、板卡序列号；
//   - SoC 身份源（全志 /sys/class/sunxi_info/sys_info、瑞芯微设备树
//     serial-number，均全局可读，见 soc.go）：平台、SID（、芯片型号、批次号）；
//   - 网卡（/sys/class/net/<档案指定的网卡>/address）：Wi-Fi MAC。
//
// 没有板卡 EEPROM 的评估板（rk3576-evb1、bcm2712-pi5、h618-x98h）只有后两个来源，
// 台账字段由档案常量（或设备树里的真实事实）补上、板卡序列号由 `--board-serial` 人工指定。
//
// 容错口径与 sysinfo 相反，这是本包最重要的一条约定：sysinfo 读不到的项
// 从快照里省略（监控数据，缺一项不影响判断），boardinfo **任一必填源读不到
// 就整体报错**——硬件诊断缺项时宁可不出一份看似完整的结果。
//
// 型号档案：按 EEPROM 里的 SKU 前缀命中档案（RS501- → cubie-a7a），档案决定
// 型号代号与 Wi-Fi 网卡名。SKU 认不出即报 [ErrUnsupportedBoard]，不猜。
// 开发机没有这套硬件，Collect 必然走到这条错误上——这是预期行为，夹具单测
// 是本包在开发机上唯一的验证途径。
//
// EEPROM 只在执行 `sudo llmgate boardinfo` 时读取；gatewayd 为固件兼容筛选只
// 识别型号，不读取或发送 EEPROM、SID、MAC 等身份值。
//
// §15.1：soc_serial 是密钥物料，包内一律用 [SIDValue] 承载，缺省打码
// （见 sid.go）。
package boardinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// SchemaID 是本地板卡诊断 JSON 的 schema 标识。
const SchemaID = "llmgate.board-info/v1"

// DefaultEEPROMPath 是板卡 EEPROM 的 sysfs 属性路径（at24 驱动挂在 i2c-0
// 地址 0x50）。等价的 nvmem 别名是 /sys/bus/nvmem/devices/0-00500/nvmem，
// 两者同为 0600 root:root。
const DefaultEEPROMPath = "/sys/bus/i2c/devices/0-0050/eeprom"

// ErrUnsupportedBoard 表示本机不是受支持的板卡：读不到板卡 EEPROM，或
// EEPROM 里的 SKU 没有对应的型号档案。
var ErrUnsupportedBoard = errors.New("无法识别板卡型号档案")

// BoardInfo 是一台设备的板卡诊断快照。字段序即 JSON 键序；新增或改变字段
// 语义时须同步提升 schema。
type BoardInfo struct {
	Schema      string   `json:"schema"`
	Model       string   `json:"model"`
	Vendor      string   `json:"vendor"`
	SKU         string   `json:"sku"`
	PCBVersion  string   `json:"pcb_version"`
	BoardSerial string   `json:"board_serial"`
	SoCPlatform string   `json:"soc_platform"`
	SoCSerial   SIDValue `json:"soc_serial"`
	SoCChiptype string   `json:"soc_chiptype"`
	SoCBatchno  string   `json:"soc_batchno"`
	WiFiMAC     string   `json:"wifi_mac"`
}

// MaskedJSON 是缺省输出：soc_serial 打码，可以随便贴进工单、日志、聊天。
func (b BoardInfo) MaskedJSON() ([]byte, error) {
	return json.MarshalIndent(b, "", "  ")
}

// profile 是一个型号档案：一块板子的身份从哪里读、读出来算哪个型号。
//
// model 是**型号代号**，且**恒等于板卡类型名**（cubie-a7a / rk3576-evb1 /
// bcm2712-pi5）：2026-08-17 产品决定量产型号未定、先不自己起名，原先那层
// 板上只保存技术型号代号，不保存营销型号名；官网固件索引也只用这个代号做
// 本地兼容筛选。
//
// 两条命中路线，对应两类板子：
//
//   - **有板卡 EEPROM**（skuPrefix 非空）：按 EEPROM 里的 SKU 前缀命中，
//     vendor/sku/pcbVersion/boardSerial 四项都从 EEPROM 读真值。量产板走这条。
//   - **无板卡 EEPROM**（dtCompatible 非空）：按设备树 compatible 命中，
//     台账字段由下面的常量补上。评估板走这条。
//
// 无 EEPROM 那条路上 vendor/sku/pcbVersion 是**设备树型号串里的真实事实**
// （"Rockchip RK3576 EVB1 V10 Board" 拆出来的），不是编造；而 socChiptype /
// socBatchno 确实**没有对应物**——那两项是全志 eFuse 的专有字段。
// 2026-08-10 产品方定：**不为此升 schema**，两项填占位串 [notApplicable]，
// 保持 llmgate-board/1 不动。代价记在这里：本包「任一必填源读不到就整体报错」
// 的口径对这两项不再成立，诊断结果里看到 "N/A" 要知道那是「读不到真值」
// 而不是「读失败」。**再加新平台之前，先回来重读这一段**：
//
//   - **本平台没有这个概念**（bcm2712-pi5，2026-08-17）：chiptype/batchno 是
//     全志 eFuse 的专有字段，博通根本没有对应物，再换个内核也读不出来。
//   - **概念在、这个内核没导出**（h618-x98h，2026-08-17）：H618 是全志芯片，
//     eFuse 里确实有芯片型号与批次号，但那两项的权威出口
//     `/sys/class/sunxi_info/sys_info` 是全志 **BSP 内核**的节点，这块板跑的
//     Armbian 主线 sunxi64 内核上压根不存在。**刻意不去裸解 eFuse 凑出这两项**：
//     偏移与编码没有厂商文档背书，猜出来的值会以「硬件真值」的身份进诊断结果，
//     比记 N/A 坏得多。哪天换回 BSP 内核，这块板该改的是 soc 字段（换回
//     [socSunxi]），不是在这里补解析。
//
// pcbVersion 有三种取法，按板子到底有没有发布改版号来选：
//
//   - **常量**（rk3576-evb1）：它的 compatible 是 `rockchip,rk3576-evb1-v10`，
//     改版号就写在匹配键里，档案里那个 "V10" 不可能与命中的板子不符。
//   - **现读设备树 model**（bcm2712-pi5，置 pcbVersionFromDTModel）：
//     `raspberrypi,5-model-b` 对 Rev 1.0/1.1 一视同仁，写死改版号等于对着
//     另一块改版的板子撒谎，所以从 "Raspberry Pi 5 Model B Rev 1.1" 尾部现取
//     （见 [Collector.readDTPCBVersion]）。
//   - **占位串 [notApplicable]**（h618-x98h）：这块板**哪里都没有**改版号——
//     compatible 是裸 `vontar,x98h`、设备树 model 是裸 "X98H"（没有 Pi 那样的
//     "Rev X.Y" 尾巴）、又没有板卡 EEPROM。三个来源都没有的时候只能记 N/A：
//     编一个常量出来就是拿造出来的事实去填一个台账字段。
//
// 同理，bcm2712-pi5 的 sku "RPI5-MODEL-B" 是**设备树 compatible 的转写**，
// 不是 Raspberry Pi 的产品编码（他们的 SC 开头编码随内存容量不同，板子上读不到）。
//
// **boardSerial 刻意没有占位常量**：它是铭牌序列号，全平台唯一。填固定占位串
// 会让第二块同型号板子得到相同诊断结果，所以无 EEPROM 的板子由操作者用
// `--board-serial` 人工指定（见 [Collector.Collect]）。
type profile struct {
	// model 既是板子类型名，也是诊断 JSON 与官网固件索引使用的型号代号。
	model     string
	wifiIface string
	soc       socKind

	// 命中方式，二选一。
	skuPrefix    string // 有 EEPROM：按 SKU 前缀
	dtCompatible string // 无 EEPROM：按设备树 compatible

	// 无 EEPROM 时的台账常量；hasEEPROM 为真时这些字段不参与。
	vendor      string
	sku         string
	pcbVersion  string
	socChiptype string
	socBatchno  string

	// pcbVersionFromDTModel 为真时 pcb_version 现读设备树 model 串尾部的
	// "Rev X.Y"，pcbVersion 常量不参与（两者互斥，取法与理由见上）。
	pcbVersionFromDTModel bool
}

// hasEEPROM 报告本档案的台账字段是否来自板卡 EEPROM。
func (p profile) hasEEPROM() bool { return p.skuPrefix != "" }

// notApplicable 是「本平台没有这个概念」的占位串。刻意不用空串：空串在
// 诊断结果里与「读失败」不可区分，而这两件事的处置完全不同。
const notApplicable = "N/A"

// profiles 是型号档案注册表，按顺序匹配（首个命中即用）。
//
// 新增一块板子时要同步官网固件索引里的 hardwareModels；两处的型号代号必须
// 逐字相同，否则设备会在本地筛掉本可安装的固件。
var profiles = []profile{
	{
		model:     "cubie-a7a",
		wifiIface: "wlan0",
		soc:       socSunxi,
		skuPrefix: "RS501-",
	},
	{
		model:        "rk3576-evb1",
		wifiIface:    "wlan0",
		soc:          socDeviceTree,
		dtCompatible: "rockchip,rk3576-evb1-v10",
		vendor:       "Rockchip",
		sku:          "RK3576-EVB1",
		pcbVersion:   "V10",
		socChiptype:  notApplicable,
		socBatchno:   notApplicable,
	},
	{
		// 树莓派 5（2026-08-17 接入的第三块开发板，见 docs-board/bcm2712-pi5/README.md）。
		// compatible 只到 `raspberrypi,5-model-b`，认不出 Pi 500 / CM5 —— 那两个
		// 是别的 compatible，照旧报 ErrUnsupportedBoard，不猜。
		model:                 "bcm2712-pi5",
		wifiIface:             "wlan0",
		soc:                   socDeviceTree,
		dtCompatible:          "raspberrypi,5-model-b",
		vendor:                "Raspberry Pi",
		sku:                   "RPI5-MODEL-B",
		pcbVersionFromDTModel: true,
		socChiptype:           notApplicable,
		socBatchno:            notApplicable,
	},
	{
		// X98H 电视盒子（2026-08-17 接入的第四块开发板，见 docs-board/h618-x98h/README.md）。
		//
		// 这是**第一块与 cubie-a7a 同厂（全志）却不走 [socSunxi] 的板子**，别顺手
		// 照着 cubie-a7a 抄：分岔点是内核而不是芯片厂商。cubie-a7a 跑全志 BSP 内核，
		// 有 /sys/class/sunxi_info/sys_info 那份 key:value；这块板跑 Armbian 主线
		// sunxi64 内核，那个节点根本不存在，能拿到的芯片身份只有设备树
		// serial-number —— 于是它与瑞芯微、树莓派共用 [socDeviceTree]。
		// **SID 因此是 64 位而不是全志惯常的 128 位**，安全口径见 [ValidSIDHexLens]
		// 与 docs-board/h618-x98h/README.md（那份文档里记着一条待办：主线内核下 128 位
		// SID 另有一个全局可读的出口，验明之前不动这里）。
		//
		// sku "X98H" 是设备树 compatible 的转写（同 bcm2712-pi5 那条口径），
		// 不是厂商的产品编码 —— 电视盒子根本没有对外发布的编码，编一个就是造事实。
		// dtCompatible 取首项 `vontar,x98h` 而不是末项 `allwinner,sun50i-h618`：
		// 后者是**芯片**，同芯片的电视盒子满地都是，拿它当匹配键会把一堆没验过的
		// 板子顺手认成这一款。
		model:        "h618-x98h",
		wifiIface:    "wlan0",
		soc:          socDeviceTree,
		dtCompatible: "vontar,x98h",
		vendor:       "Vontar",
		sku:          "X98H",
		pcbVersion:   notApplicable,
		socChiptype:  notApplicable,
		socBatchno:   notApplicable,
	},
}

// matchProfile 按 SKU 前缀查有 EEPROM 的档案。
func matchProfile(sku string) (profile, bool) {
	for _, p := range profiles {
		if p.hasEEPROM() && strings.HasPrefix(sku, p.skuPrefix) {
			return p, true
		}
	}
	return profile{}, false
}

// detectProfile 认出本机是哪块板子，不需要 root。
//
// 先试无 EEPROM 的档案（按设备树 compatible，全局可读），再回落到有 EEPROM 的
// 档案（按 SoC 身份源是否读得到）。**刻意不读 EEPROM**：这条路要给 gatewayd
// 运行期用，而 EEPROM 是 root only。
func (c *Collector) detectProfile() (profile, bool) {
	p, _, ok := c.detectProfileSoC()
	return p, ok
}

// detectProfileSoC 与 detectProfile 同一探测顺序，并把有 EEPROM 档案在探测时
// 已读到的 SoC 身份捎带回来（nil = 无 EEPROM 档案按设备树命中，SoC 待调用方
// 按档案自读）：Collect 需要 SoC 诊断信息，探测已经读过一遍就不必立刻再读
// 第二遍。仍然不跨调用缓存。
func (c *Collector) detectProfileSoC() (profile, *socIdentity, bool) {
	if p, ok := c.detectNoEEPROMProfile(); ok {
		return p, nil, true
	}
	for _, p := range profiles {
		if p.hasEEPROM() {
			if soc, err := c.readSoCBy(p.soc); err == nil {
				return p, &soc, true
			}
		}
	}
	return profile{}, nil, false
}

// Collector 读取板卡身份。零值不可用，必须经 [NewCollector] 构造；
// 两个路径字段是小写的，单测按 sysinfo 的老办法注入夹具目录。
type Collector struct {
	sysRoot    string // "/sys"
	eepromPath string // EEPROM sysfs 属性（root only）
}

// NewCollector 构造读取真实 /sys 与板卡 EEPROM 的采集器。
func NewCollector() *Collector {
	return &Collector{sysRoot: "/sys", eepromPath: DefaultEEPROMPath}
}

// Collect 读齐三个来源并装配 [BoardInfo]。任一必填项缺失即返回错误。
//
// 错误可用 errors.Is 判别两类调用方关心的情形：[ErrUnsupportedBoard]
// （不是受支持的板卡，开发机即此类）与 os.ErrPermission（EEPROM 要 root，
// 提示改用 sudo）。
func (c *Collector) Collect(boardSerial string) (*BoardInfo, error) {
	boardSerial = strings.TrimSpace(boardSerial)

	// 先认无 EEPROM 的板子：这条只读全局可读的设备树，不需要 root，也不该
	// 因为「读不到 EEPROM」而被归成「不是受支持的板卡」。
	if prof, ok := c.detectNoEEPROMProfile(); ok {
		return c.collectWithoutEEPROM(prof, boardSerial)
	}

	raw, err := os.ReadFile(c.eepromPath)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			// 权限问题是「有这块板但没权限」，不能归到「不是这块板」。
			return nil, fmt.Errorf("读取板卡 EEPROM %s: %w", c.eepromPath, err)
		}
		return nil, fmt.Errorf("%w：读取板卡 EEPROM %s: %w", ErrUnsupportedBoard, c.eepromPath, err)
	}
	ident, err := readEEPROMIdentity(raw)
	if err != nil {
		return nil, err
	}
	prof, ok := matchProfile(ident.sku)
	if !ok {
		return nil, fmt.Errorf("%w：SKU %q 没有对应档案", ErrUnsupportedBoard, ident.sku)
	}
	// 认出是有 EEPROM 的板子之后才谈人工序列号：认不出的板子应当报
	// ErrUnsupportedBoard，那是调用方分辨「不是这块板」的依据，不能被这条
	// 参数校验抢先。序列号只认 EEPROM 里的真值——默默忽略人工值会让操作者
	// 误以为诊断结果采用了传入值。
	if boardSerial != "" {
		return nil, errors.New("--board-serial 只用于没有板卡 EEPROM 的型号；本机的板卡序列号来自 EEPROM，不接受人工指定")
	}

	soc, err := c.readSoCBy(prof.soc)
	if err != nil {
		return nil, err
	}
	mac, err := c.readMAC(prof.wifiIface)
	if err != nil {
		return nil, err
	}

	return &BoardInfo{
		Schema:      SchemaID,
		Model:       prof.model,
		Vendor:      ident.vendor,
		SKU:         ident.sku,
		PCBVersion:  ident.pcbVersion,
		BoardSerial: ident.boardSerial,
		SoCPlatform: soc.platform,
		SoCSerial:   soc.sid,
		SoCChiptype: soc.chiptype,
		SoCBatchno:  soc.batchno,
		WiFiMAC:     mac,
	}, nil
}

// detectNoEEPROMProfile 按设备树 compatible 认无 EEPROM 的档案。
func (c *Collector) detectNoEEPROMProfile() (profile, bool) {
	for _, p := range profiles {
		if !p.hasEEPROM() && c.matchDTCompatible(p.dtCompatible) {
			return p, true
		}
	}
	return profile{}, false
}

// pcbVersion 取本档案的板卡改版号：多数档案是常量，声明了
// pcbVersionFromDTModel 的档案现读设备树 model（两种取法的分工见 [profile]）。
func (c *Collector) pcbVersion(p profile) (string, error) {
	if !p.pcbVersionFromDTModel {
		return p.pcbVersion, nil
	}
	return c.readDTPCBVersion()
}

// collectWithoutEEPROM 装配没有板卡 EEPROM 的板子的身份。
//
// 台账四项来自档案常量，板卡序列号必须由产线人工给出——理由见 [profile]
// 的注释：固定占位串会让同型号的第二块板撞号。
func (c *Collector) collectWithoutEEPROM(prof profile, boardSerial string) (*BoardInfo, error) {
	if boardSerial == "" {
		return nil, fmt.Errorf("%s 没有板卡 EEPROM，板卡序列号必须人工指定："+
			"请加 --board-serial=<序列号>（同型号的每块板必须不同）", prof.model)
	}
	if strings.ContainsFunc(boardSerial, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, errors.New("--board-serial 不得含控制字符")
	}

	soc, err := c.readSoCBy(prof.soc)
	if err != nil {
		return nil, err
	}
	mac, err := c.readMAC(prof.wifiIface)
	if err != nil {
		return nil, err
	}
	pcbVersion, err := c.pcbVersion(prof)
	if err != nil {
		return nil, err
	}

	// chiptype/batchno 取档案的占位串：本平台没有这两个概念，不是读失败。
	chiptype, batchno := soc.chiptype, soc.batchno
	if chiptype == "" {
		chiptype = prof.socChiptype
	}
	if batchno == "" {
		batchno = prof.socBatchno
	}

	return &BoardInfo{
		Schema:      SchemaID,
		Model:       prof.model,
		Vendor:      prof.vendor,
		SKU:         prof.sku,
		PCBVersion:  pcbVersion,
		BoardSerial: boardSerial,
		SoCPlatform: soc.platform,
		SoCSerial:   soc.sid,
		SoCChiptype: chiptype,
		SoCBatchno:  batchno,
		WiFiMAC:     mac,
	}, nil
}

// Model 返回本机的技术型号代号，不读取或暴露 SID。官网固件索引用它在设备
// 本地筛选兼容制品；它不是设备身份，也不会随请求发送到官网。
func (c *Collector) Model() (string, error) {
	prof, _, ok := c.detectProfileSoC()
	if !ok {
		return "", fmt.Errorf("%w：设备树与各 SoC 身份源都认不出本机平台", ErrUnsupportedBoard)
	}
	return prof.model, nil
}

// socIdentity 是 sunxi_info 提供的四项。
type socIdentity struct {
	platform string
	sid      SIDValue
	chiptype string
	batchno  string
}

// sunxiInfoPath 返回 SoC 信息节点路径。
func (c *Collector) sunxiInfoPath() string {
	return filepath.Join(c.sysRoot, "class", "sunxi_info", "sys_info")
}

// readSunxiSoC 解析 /sys/class/sunxi_info/sys_info：
//
//	sunxi_platform    : A733
//	sunxi_secure      : normal
//	sunxi_serial      : <32 位小写 hex>
//	sunxi_chiptype    : 00005100
//	sunxi_batchno     : 0x19030001
//
// 四项必填全部缺一不可（sunxi_secure/sunxi_soc_ver 不进 JSON）。
func (c *Collector) readSunxiSoC() (socIdentity, error) {
	path := c.sunxiInfoPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return socIdentity{}, fmt.Errorf("读取 SoC 信息 %s: %w", path, err)
		}
		return socIdentity{}, fmt.Errorf("%w：读取 SoC 信息 %s: %w", ErrUnsupportedBoard, path, err)
	}

	kv := make(map[string]string, 6)
	for line := range strings.SplitSeq(string(data), "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		kv[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}

	var soc socIdentity
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"sunxi_platform", &soc.platform},
		{"sunxi_chiptype", &soc.chiptype},
		{"sunxi_batchno", &soc.batchno},
	} {
		v := kv[f.key]
		if v == "" {
			return socIdentity{}, fmt.Errorf("SoC 信息 %s 缺少 %s", path, f.key)
		}
		*f.dst = v
	}

	// 错误里带上字段名与形状，但绝不回显读到的值（§15.1）。
	sid, err := ParseSID(kv["sunxi_serial"])
	if err != nil {
		return socIdentity{}, fmt.Errorf("SoC 信息 %s 的 sunxi_serial 非法: %w", path, err)
	}
	soc.sid = sid
	return soc, nil
}

// readMAC 读档案指定网卡的 MAC 并规范化为小写冒号形式。
func (c *Collector) readMAC(iface string) (string, error) {
	path := filepath.Join(c.sysRoot, "class", "net", iface, "address")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取网卡 %s 的 MAC 地址 %s: %w", iface, path, err)
	}
	hw, err := net.ParseMAC(strings.TrimSpace(string(data)))
	if err != nil {
		return "", fmt.Errorf("网卡 %s 的 MAC 地址 %s 格式非法: %w", iface, path, err)
	}
	return hw.String(), nil
}
