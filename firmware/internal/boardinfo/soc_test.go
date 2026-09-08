package boardinfo

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 假 SID：16 位 hex，形如瑞芯微 serial-number。真实 SID 是密钥物料，
// 本仓库任何文件里都只出现假值（§15.1）。
const fakeRKSID = "0123456789abcdef"

// newRK3576Fixture 在临时目录里搭出一块 rk3576-evb1 的 sysfs。
//
// **夹具必须运行期生成，不能是提交进仓库的文件**：设备树的字符串属性以
// NUL 结尾（compatible 更是多个 NUL 分隔的串），把裸控制字节写进仓库文件
// 既容易被编辑器/工具悄悄改掉，也读不出这里想验的那件事——readDTString
// 到底有没有按 NUL 切分。
func newRK3576Fixture(t *testing.T, serial string) *Collector {
	t.Helper()
	root := t.TempDir()

	dt := filepath.Join(root, "firmware", "devicetree", "base")
	if err := os.MkdirAll(dt, 0o755); err != nil {
		t.Fatalf("建设备树目录: %v", err)
	}
	// 设备树字符串属性：单串也带结尾 NUL。
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dt, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	write("serial-number", serial+"\x00")
	// compatible 是 NUL 分隔的多串，从最具体排到最泛。
	write("compatible", "rockchip,rk3576-evb1-v10\x00rockchip,rk3576\x00")

	nic := filepath.Join(root, "class", "net", "wlan0")
	if err := os.MkdirAll(nic, 0o755); err != nil {
		t.Fatalf("建网卡目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nic, "address"), []byte("24:3f:75:a2:86:62\n"), 0o644); err != nil {
		t.Fatalf("写 MAC: %v", err)
	}

	// eepromPath 指向不存在的路径：这块板没有板卡 EEPROM，采集不该去碰它。
	return &Collector{sysRoot: root, eepromPath: filepath.Join(root, "no-such-eeprom")}
}

// TestRK3576Collect 是无 EEPROM 路线的主断言：台账字段取档案常量，板卡序列号
// 取人工值，SoC 两项取设备树。
func TestRK3576Collect(t *testing.T) {
	c := newRK3576Fixture(t, fakeRKSID)
	info, err := c.Collect("EVB-RK3576-001")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, tc := range []struct{ field, got, want string }{
		{"schema", info.Schema, SchemaID},
		{"model", info.Model, "rk3576-evb1"},
		{"vendor", info.Vendor, "Rockchip"},
		{"sku", info.SKU, "RK3576-EVB1"},
		{"pcb_version", info.PCBVersion, "V10"},
		{"board_serial", info.BoardSerial, "EVB-RK3576-001"},
		// 平台名取 compatible 的**末项**（rockchip,rk3576），不是板级型号。
		{"soc_platform", info.SoCPlatform, "RK3576"},
		{"soc_chiptype", info.SoCChiptype, notApplicable},
		{"soc_batchno", info.SoCBatchno, notApplicable},
		{"wifi_mac", info.WiFiMAC, "24:3f:75:a2:86:62"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，want %q", tc.field, tc.got, tc.want)
		}
	}
	if got := info.SoCSerial.sid; got != fakeRKSID {
		t.Errorf("soc_serial = %q，want %q", got, fakeRKSID)
	}
}

// TestRK3576SIDNeverLeaks 盯住无 EEPROM 这条新路上的 §15.1：缺省序列化、
// String、LogValue 三条出口都不许吐完整 SID。
func TestRK3576SIDNeverLeaks(t *testing.T) {
	c := newRK3576Fixture(t, fakeRKSID)
	info, err := c.Collect("EVB-RK3576-001")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	masked, err := info.MaskedJSON()
	if err != nil {
		t.Fatalf("MaskedJSON: %v", err)
	}
	if strings.Contains(string(masked), fakeRKSID) {
		t.Error("MaskedJSON 里出现了完整 SID")
	}
	if strings.Contains(info.SoCSerial.String(), fakeRKSID) {
		t.Error("String() 吐出了完整 SID")
	}
	if strings.Contains(info.SoCSerial.LogValue().String(), fakeRKSID) {
		t.Error("LogValue() 吐出了完整 SID")
	}

}

// TestBoardSerialRequired 盯住那条不能退让的约束：无 EEPROM 的板子不给
// 板卡序列号就必须报错，绝不落一个固定占位串——那会让同型号第二块板撞号。
func TestBoardSerialRequired(t *testing.T) {
	c := newRK3576Fixture(t, fakeRKSID)
	for _, serial := range []string{"", "   ", "\t"} {
		if _, err := c.Collect(serial); err == nil {
			t.Errorf("Collect(%q) 应当报错", serial)
		}
	}
	if _, err := c.Collect("A\x00B"); err == nil {
		t.Error("含控制字符的板卡序列号应当被拒")
	}
}

// TestBoardSerialRejectedOnEEPROMBoard 反方向：有 EEPROM 的板子不接受人工
// 序列号。默默忽略会让操作者以为改掉了诊断序列号，实际并没有。
func TestBoardSerialRejectedOnEEPROMBoard(t *testing.T) {
	if _, err := testCollector().Collect("MANUAL-1"); err == nil {
		t.Fatal("有 EEPROM 的板子应当拒绝 --board-serial")
	}
}

// TestRKUnknownBoardIsUnsupported 认不出的瑞芯微板子要归到 ErrUnsupportedBoard，
// 而不是别的错误类别——调用方靠这个分辨「不是这块板」。
func TestRKUnknownBoardIsUnsupported(t *testing.T) {
	c := newRK3576Fixture(t, fakeRKSID)
	dt := filepath.Join(c.sysRoot, "firmware", "devicetree", "base")
	if err := os.WriteFile(filepath.Join(dt, "compatible"), []byte("rockchip,rk3588-evb1-v10\x00rockchip,rk3588\x00"), 0o644); err != nil {
		t.Fatalf("改 compatible: %v", err)
	}
	if _, err := c.Collect("EVB-1"); !errors.Is(err, ErrUnsupportedBoard) {
		t.Errorf("err = %v，want ErrUnsupportedBoard", err)
	}
}

// TestBadRKSerial 设备树里的序列号形状不对时要报错，且错误文本不回显值。
func TestBadRKSerial(t *testing.T) {
	for _, bad := range []string{"", "xyz", "0123456789abcde", "0123456789abcdefz"} {
		c := newRK3576Fixture(t, bad)
		_, err := c.Collect("EVB-RK3576-001")
		if err == nil {
			t.Errorf("serial %q 应当被拒", bad)
			continue
		}
		if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("错误文本回显了序列号：%v", err)
		}
	}
}

// TestReadDTStringSplitsOnNUL 直接盯住 NUL 切分——compatible 的末项取错，
// 平台名就会变成板级型号，换块同芯片的板子即漂移。
func TestReadDTStringSplitsOnNUL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "compatible")
	if err := os.WriteFile(p, []byte("a,b\x00c,d\x00"), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	got, err := readDTString(p)
	if err != nil {
		t.Fatalf("readDTString: %v", err)
	}
	if len(got) != 2 || got[0] != "a,b" || got[1] != "c,d" {
		t.Errorf("readDTString = %q，want [a,b c,d]", got)
	}
}

// 假 SID：树莓派那条也是 16 位 hex（同 [socDeviceTree] 路线）。真实 SID 是
// 密钥物料，本仓库任何文件里都只出现假值（§15.1）。
const fakePiSID = "fedcba9876543210"

// pi5DTModel 是树莓派 5 设备树 model 串的真实形状（改版号在末尾）。
const pi5DTModel = "Raspberry Pi 5 Model B Rev 1.1"

// newPi5Fixture 在临时目录里搭出一块 bcm2712-pi5 的 sysfs。
// dtModel 单独传是因为板卡改版号现读自它——见 TestPi5PCBVersionTracksDTModel。
func newPi5Fixture(t *testing.T, serial, dtModel string) *Collector {
	t.Helper()
	root := t.TempDir()

	dt := filepath.Join(root, "firmware", "devicetree", "base")
	if err := os.MkdirAll(dt, 0o755); err != nil {
		t.Fatalf("建设备树目录: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dt, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	write("serial-number", serial+"\x00")
	// 末项是 SoC（brcm,bcm2712），首项是板级型号——平台名取末项。
	write("compatible", "raspberrypi,5-model-b\x00brcm,bcm2712\x00")
	write("model", dtModel+"\x00")

	nic := filepath.Join(root, "class", "net", "wlan0")
	if err := os.MkdirAll(nic, 0o755); err != nil {
		t.Fatalf("建网卡目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nic, "address"), []byte("2c:cf:67:12:34:56\n"), 0o644); err != nil {
		t.Fatalf("写 MAC: %v", err)
	}

	// 这块板同样没有板卡 EEPROM，采集不该去碰它。
	return &Collector{sysRoot: root, eepromPath: filepath.Join(root, "no-such-eeprom")}
}

// TestPi5Collect 是第三种平台的主断言：与 RK3576 同一条无 EEPROM 路线，
// 差别只在 pcb_version 现读设备树而不取常量。
func TestPi5Collect(t *testing.T) {
	c := newPi5Fixture(t, fakePiSID, pi5DTModel)
	info, err := c.Collect("PI5-DEV-001")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, tc := range []struct{ field, got, want string }{
		{"schema", info.Schema, SchemaID},
		{"model", info.Model, "bcm2712-pi5"},
		{"vendor", info.Vendor, "Raspberry Pi"},
		{"sku", info.SKU, "RPI5-MODEL-B"},
		{"pcb_version", info.PCBVersion, "1.1"},
		{"board_serial", info.BoardSerial, "PI5-DEV-001"},
		// 平台名取 compatible 的**末项**（brcm,bcm2712），不是板级型号。
		{"soc_platform", info.SoCPlatform, "BCM2712"},
		{"soc_chiptype", info.SoCChiptype, notApplicable},
		{"soc_batchno", info.SoCBatchno, notApplicable},
		{"wifi_mac", info.WiFiMAC, "2c:cf:67:12:34:56"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，want %q", tc.field, tc.got, tc.want)
		}
	}
	if got := info.SoCSerial.sid; got != fakePiSID {
		t.Errorf("soc_serial = %q，want %q", got, fakePiSID)
	}
}

// TestPi5PCBVersionTracksDTModel 是「pcb_version 不是常量」的绊线：
// raspberrypi,5-model-b 这个匹配键对 Rev 1.0/1.1 一视同仁，写死改版号就会
// 对着另一块改版的板子撒谎。
func TestPi5PCBVersionTracksDTModel(t *testing.T) {
	for _, tc := range []struct{ dtModel, want string }{
		{"Raspberry Pi 5 Model B Rev 1.0", "1.0"},
		{"Raspberry Pi 5 Model B Rev 1.1", "1.1"},
		{"Raspberry Pi 5 Model B Rev 2.3", "2.3"},
	} {
		info, err := newPi5Fixture(t, fakePiSID, tc.dtModel).Collect("PI5-DEV-001")
		if err != nil {
			t.Fatalf("Collect(%q): %v", tc.dtModel, err)
		}
		if info.PCBVersion != tc.want {
			t.Errorf("model %q → pcb_version = %q，want %q", tc.dtModel, info.PCBVersion, tc.want)
		}
	}
}

// TestPi5BadDTModel：model 串认不出改版号就整体报错，不编一个、也不产半份
// 文档——这一项是必填源，与 chiptype/batchno 那两项「本平台无此概念」不同。
func TestPi5BadDTModel(t *testing.T) {
	for _, bad := range []string{"Raspberry Pi 5 Model B", "Rev", "Raspberry Pi 5 Model B Rev"} {
		c := newPi5Fixture(t, fakePiSID, bad)
		if info, err := c.Collect("PI5-DEV-001"); err == nil {
			t.Errorf("model %q 应当被拒，却产出了 %+v", bad, info)
		}
	}
}

// TestPi5SiblingBoardIsUnsupported：同厂别的板子（Pi 500 / CM5 是另外的
// compatible）不许被这份档案顺手认走——认不出就报 ErrUnsupportedBoard，不猜。
func TestPi5SiblingBoardIsUnsupported(t *testing.T) {
	c := newPi5Fixture(t, fakePiSID, "Raspberry Pi 500 Rev 1.0")
	dt := filepath.Join(c.sysRoot, "firmware", "devicetree", "base")
	if err := os.WriteFile(filepath.Join(dt, "compatible"), []byte("raspberrypi,500\x00brcm,bcm2712\x00"), 0o644); err != nil {
		t.Fatalf("改 compatible: %v", err)
	}
	if _, err := c.Collect("PI500-1"); !errors.Is(err, ErrUnsupportedBoard) {
		t.Errorf("err = %v，want ErrUnsupportedBoard", err)
	}
}

// 假 SID：H618 这条也是 16 位 hex（同 [socDeviceTree] 路线）。真实 SID 是
// 密钥物料，本仓库任何文件里都只出现假值（§15.1）。
const fakeH618SID = "89abcdef01234567"

// newX98HFixture 在临时目录里搭出一块 h618-x98h 的 sysfs。
//
// **夹具刻意不建 /sys/class/sunxi_info**：这块板与 cubie-a7a 同为全志，却跑
// 主线内核、没有那个 BSP 节点。夹具里少了它，就把「这块板必须走设备树而不是
// socSunxi」变成了跑得起来的断言——照 cubie-a7a 抄一份档案过来的话，这里的
// 每个 Collect 都会失败。
func newX98HFixture(t *testing.T, serial, dtModel string) *Collector {
	t.Helper()
	root := t.TempDir()

	dt := filepath.Join(root, "firmware", "devicetree", "base")
	if err := os.MkdirAll(dt, 0o755); err != nil {
		t.Fatalf("建设备树目录: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dt, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s: %v", name, err)
		}
	}
	write("serial-number", serial+"\x00")
	// 末项是 SoC（allwinner,sun50i-h618），首项是板级型号——档案按首项命中，
	// 平台名取末项。
	write("compatible", "vontar,x98h\x00allwinner,sun50i-h618\x00")
	// 裸型号串，没有 "Rev X.Y" 尾巴——这正是这块板 pcb_version 只能记 N/A 的原因。
	write("model", dtModel+"\x00")

	nic := filepath.Join(root, "class", "net", "wlan0")
	if err := os.MkdirAll(nic, 0o755); err != nil {
		t.Fatalf("建网卡目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nic, "address"), []byte("88:00:33:aa:bb:cc\n"), 0o644); err != nil {
		t.Fatalf("写 MAC: %v", err)
	}

	// 这块板同样没有板卡 EEPROM，采集不该去碰它。
	return &Collector{sysRoot: root, eepromPath: filepath.Join(root, "no-such-eeprom")}
}

// TestX98HCollect 是第四种平台的主断言：无 EEPROM 路线，且 pcb_version 走的是
// 第三种取法（占位串）——这块板哪里都没有发布改版号。
func TestX98HCollect(t *testing.T) {
	c := newX98HFixture(t, fakeH618SID, "X98H")
	info, err := c.Collect("X98H-DEV-001")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	for _, tc := range []struct{ field, got, want string }{
		{"schema", info.Schema, SchemaID},
		{"model", info.Model, "h618-x98h"},
		{"vendor", info.Vendor, "Vontar"},
		{"sku", info.SKU, "X98H"},
		// 三个来源都没有改版号，只能记占位串——不编一个常量。
		{"pcb_version", info.PCBVersion, notApplicable},
		{"board_serial", info.BoardSerial, "X98H-DEV-001"},
		// 平台名取 compatible 的**末项**（allwinner,sun50i-h618），不是板级型号。
		{"soc_platform", info.SoCPlatform, "SUN50I-H618"},
		{"soc_chiptype", info.SoCChiptype, notApplicable},
		{"soc_batchno", info.SoCBatchno, notApplicable},
		{"wifi_mac", info.WiFiMAC, "88:00:33:aa:bb:cc"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，want %q", tc.field, tc.got, tc.want)
		}
	}
	if got := info.SoCSerial.sid; got != fakeH618SID {
		t.Errorf("soc_serial = %q，want %q", got, fakeH618SID)
	}
}

// TestX98HDoesNotNeedSunxiInfo 把上面夹具注释里那条约定钉成显式断言：这块板
// 不碰 /sys/class/sunxi_info 也能认出官网固件筛选所需的型号。
func TestX98HDoesNotNeedSunxiInfo(t *testing.T) {
	c := newX98HFixture(t, fakeH618SID, "X98H")
	if _, err := os.Stat(c.sunxiInfoPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("夹具里不该有 sunxi_info：stat err = %v", err)
	}
	model, err := c.Model()
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	if model != "h618-x98h" {
		t.Errorf("Model = %q，want h618-x98h", model)
	}
}

// TestX98HSiblingBoxIsUnsupported：同芯片的别款电视盒子不许被这份档案顺手认走。
// 匹配键是**板级** compatible（vontar,x98h）而不是芯片（allwinner,sun50i-h618）——
// 用芯片当键会把满地都是的 H618 盒子全认成这一款，而它们谁都没验过。
func TestX98HSiblingBoxIsUnsupported(t *testing.T) {
	c := newX98HFixture(t, fakeH618SID, "Some Other H618 Box")
	dt := filepath.Join(c.sysRoot, "firmware", "devicetree", "base")
	if err := os.WriteFile(filepath.Join(dt, "compatible"), []byte("other,h618-box\x00allwinner,sun50i-h618\x00"), 0o644); err != nil {
		t.Fatalf("改 compatible: %v", err)
	}
	if _, err := c.Collect("BOX-1"); !errors.Is(err, ErrUnsupportedBoard) {
		t.Errorf("err = %v，want ErrUnsupportedBoard", err)
	}
}

// TestProfilePCBVersionSourcesAreExclusive：pcb_version 的两种取法互斥，且
// 无 EEPROM 的档案必须有其中一种。两个都写会让常量被静默忽略、一个都不写会
// 让诊断结果里少一项——两种都是只有拿到真板才看得出来的那类错。
func TestProfilePCBVersionSourcesAreExclusive(t *testing.T) {
	for _, p := range profiles {
		if p.pcbVersionFromDTModel && p.pcbVersion != "" {
			t.Errorf("档案 %s 同时写了 pcbVersion 常量与 pcbVersionFromDTModel", p.model)
		}
		if !p.hasEEPROM() && !p.pcbVersionFromDTModel && p.pcbVersion == "" {
			t.Errorf("档案 %s 没有板卡 EEPROM，却没给 pcb_version 任何来源", p.model)
		}
	}
}

// TestValidSIDHexLens 两种受支持板卡位宽都要收，别的一律拒。
func TestValidSIDHexLens(t *testing.T) {
	for _, ok := range []string{
		strings.Repeat("a", 16), // 瑞芯微 64 位
		strings.Repeat("a", 32), // 全志 128 位
		strings.ToUpper(strings.Repeat("ab", 8)),
	} {
		if _, err := ParseSID(ok); err != nil {
			t.Errorf("ParseSID(%d 位) 应当通过: %v", len(ok), err)
		}
	}
	for _, bad := range []string{
		strings.Repeat("a", 8),
		strings.Repeat("a", 15),
		strings.Repeat("a", 17),
		strings.Repeat("a", 31),
		strings.Repeat("a", 33),
		strings.Repeat("a", 64),
	} {
		if _, err := ParseSID(bad); err == nil {
			t.Errorf("ParseSID(%d 位) 应当被拒", len(bad))
		}
	}
}
