package boardinfo

// 夹具驱动的板卡身份测试。开发机没有这套硬件，夹具是本包唯一的验证途径：
//
//   - testdata/eeprom.bin 是 2026-08-07 从 cubie-a7a-dev 原样导出的 2048 字节
//     EEPROM 镜像（真实布局，含真实厂商/SKU/PCB 版本/板卡序列号，均非秘密）；
//   - testdata/sys/ 是仿真 /sys：sunxi_info 里的 sunxi_serial 是**假 SID**
//     （0123456789abcdef…），仓库任何文件都不许出现真实 SID（§15.1）；
//   - testdata/golden-*.json 锁死键序与打码行为，两侧契约靠它固定。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSID 是夹具里的假 SID，与 testdata/sys/class/sunxi_info/sys_info 一致。
const fakeSID = "0123456789abcdef0123456789abcdef"

// testCollector 返回读夹具的采集器。
func testCollector() *Collector {
	return &Collector{sysRoot: "testdata/sys", eepromPath: "testdata/eeprom.bin"}
}

// TestCollectGolden 是本包的主断言：真实布局的 EEPROM + 假 SID 的 sunxi_info
// 采出的 JSON 必须逐字节等于 golden——键序、打码、字段口径一处都不许漂。
func TestCollectGolden(t *testing.T) {
	info, err := testCollector().Collect("")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got, err := info.MaskedJSON()
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	const golden = "testdata/golden-masked.json"
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("读 golden: %v", err)
	}
	if string(got) != strings.TrimRight(string(want), "\n") {
		t.Errorf("与 %s 不符：\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

// TestCollectFields 逐项核对采集结果，含三个来源各自的字段归属。
func TestCollectFields(t *testing.T) {
	info, err := testCollector().Collect("")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"schema", info.Schema, SchemaID},
		{"model", info.Model, "cubie-a7a"},              // 型号代号 = 板卡类型名
		{"vendor", info.Vendor, "Radxa Computer"},       // EEPROM 里带右对齐空格，须裁掉
		{"sku", info.SKU, "RS501-D4S8R42W28"},           // EEPROM
		{"pcb_version", info.PCBVersion, "V1.10D"},      // EEPROM
		{"board_serial", info.BoardSerial, "B697657O"},  // EEPROM
		{"soc_platform", info.SoCPlatform, "A733"},      // sunxi_info
		{"soc_chiptype", info.SoCChiptype, "00005100"},  // sunxi_info
		{"soc_batchno", info.SoCBatchno, "0x19030001"},  // sunxi_info
		{"wifi_mac", info.WiFiMAC, "9c:04:b6:84:3d:72"}, // 网卡
		{"soc_serial", info.SoCSerial.sid, fakeSID},     // sunxi_info；只在本包测试内核对
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestSIDNeverLeaks 是 §15.1 的纵深防御断言：SID 不许出现在任何字符串化路径上。
func TestSIDNeverLeaks(t *testing.T) {
	info, err := testCollector().Collect("")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	masked, err := info.MaskedJSON()
	if err != nil {
		t.Fatalf("MaskedJSON: %v", err)
	}

	// 整份 BoardInfo 误进 logger 也不该吐出 SID。
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})).
		Debug("board", "info", info, "sid", info.SoCSerial)

	for name, s := range map[string]string{
		"MaskedJSON":      string(masked),
		"SIDValue.String": info.SoCSerial.String(),
		"fmt %v":          info.SoCSerial.String(),
		"slog 输出":         buf.String(),
	} {
		if strings.Contains(s, fakeSID) {
			t.Errorf("%s 泄露了完整 SID: %s", name, s)
		}
	}

	// 打码值保留 4 位前缀便于人工比对，且不带可离线校验的摘要。
	if got := info.SoCSerial.String(); got != fakeSID[:4]+"…" {
		t.Errorf("打码形态 = %q，want %q", got, fakeSID[:4]+"…")
	}

	again, err := info.MaskedJSON()
	if err != nil {
		t.Fatalf("MaskedJSON: %v", err)
	}
	if !bytes.Equal(again, masked) {
		t.Errorf("重复 MaskedJSON 的结果发生变化：%s", again)
	}
}

// TestParseSID 覆盖形状校验，并确认错误文本不回显输入。
func TestParseSID(t *testing.T) {
	if _, err := ParseSID("0123456789ABCDEF0123456789ABCDEF"); err != nil {
		t.Errorf("大写 hex 应被规范化接受: %v", err)
	}
	for name, in := range map[string]string{
		"太短":    fakeSID[:31],
		"太长":    fakeSID + "0",
		"非 hex": strings.Repeat("g", 32),
		"空":     "",
	} {
		err := func() error { _, err := ParseSID(in); return err }()
		if err == nil {
			t.Errorf("%s 应被拒绝", name)
			continue
		}
		if in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("%s 的错误文本回显了输入: %v", name, err)
		}
	}
}

// TestEEPROMWalksByHeader 证明解析走的是记录头链而不是死偏移：把第一条
// 记录（产品记录）加长若干字节后重算 CRC，各字段仍须完整取出。
func TestEEPROMWalksByHeader(t *testing.T) {
	raw := readFixtureEEPROM(t)
	grown := growFirstRecord(t, raw, 16)

	dir := t.TempDir()
	path := filepath.Join(dir, "eeprom.bin")
	if err := os.WriteFile(path, grown, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Collector{sysRoot: "testdata/sys", eepromPath: path}
	info, err := c.Collect("")
	if err != nil {
		t.Fatalf("记录整体后移后仍应解析成功: %v", err)
	}
	if info.PCBVersion != "V1.10D" || info.BoardSerial != "B697657O" {
		t.Errorf("后续记录解析错位: pcb=%q serial=%q", info.PCBVersion, info.BoardSerial)
	}
}

// TestEEPROMCRC 确认坏块会被拒绝，而不是把半截值当真。
func TestEEPROMCRC(t *testing.T) {
	raw := readFixtureEEPROM(t)
	raw[0x4a] ^= 0xff // 打坏 SKU 首字节，CRC 随之不符

	if _, err := readEEPROMIdentity(raw); !errors.Is(err, ErrEEPROMFormat) {
		t.Errorf("CRC 不符应报 ErrEEPROMFormat，得到 %v", err)
	}
}

// TestEEPROMRejectsGarbage 覆盖魔数/版本/越界三类畸形。
func TestEEPROMRejectsGarbage(t *testing.T) {
	raw := readFixtureEEPROM(t)
	cases := map[string]func([]byte){
		"魔数不符":   func(b []byte) { copy(b[:4], "XXXX") },
		"版本不支持":  func(b []byte) { binary.LittleEndian.PutUint16(b[4:6], 99) },
		"记录长度越界": func(b []byte) { binary.LittleEndian.PutUint16(b[0x10:0x12], 0xfff0) },
		"记录数为零":  func(b []byte) { binary.LittleEndian.PutUint16(b[6:8], 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b := bytes.Clone(raw)
			mutate(b)
			if _, err := readEEPROMIdentity(b); !errors.Is(err, ErrEEPROMFormat) {
				t.Errorf("应报 ErrEEPROMFormat，得到 %v", err)
			}
		})
	}
}

// TestUnknownSKU 确认 SKU 认不出就报错，不猜一个档案顶上。
func TestUnknownSKU(t *testing.T) {
	raw := readFixtureEEPROM(t)
	copy(raw[0x4a:], "ZZ999-")
	fixRecordCRCs(t, raw)

	dir := t.TempDir()
	path := filepath.Join(dir, "eeprom.bin")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Collector{sysRoot: "testdata/sys", eepromPath: path}
	_, err := c.Collect("")
	if !errors.Is(err, ErrUnsupportedBoard) {
		t.Fatalf("未知 SKU 应报 ErrUnsupportedBoard，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "ZZ999-") {
		t.Errorf("错误应指名 SKU 便于排查: %v", err)
	}
}

// TestMissingSources 确认必填源缺失一律报错——与 sysinfo「读不到就省略」
// 的口径相反（决策 1）。
func TestMissingSources(t *testing.T) {
	t.Run("无 EEPROM（开发机）", func(t *testing.T) {
		c := &Collector{sysRoot: "testdata/sys", eepromPath: filepath.Join(t.TempDir(), "nope")}
		if _, err := c.Collect(""); !errors.Is(err, ErrUnsupportedBoard) {
			t.Fatalf("want ErrUnsupportedBoard，得到 %v", err)
		}
	})

	t.Run("无 sunxi_info", func(t *testing.T) {
		c := &Collector{sysRoot: t.TempDir(), eepromPath: "testdata/eeprom.bin"}
		if _, err := c.Collect(""); !errors.Is(err, ErrUnsupportedBoard) {
			t.Fatalf("want ErrUnsupportedBoard，得到 %v", err)
		}
	})

	t.Run("sunxi_info 缺字段", func(t *testing.T) {
		sys := copySysFixture(t, "sunxi_batchno     : 0x19030001\n", "")
		c := &Collector{sysRoot: sys, eepromPath: "testdata/eeprom.bin"}
		if _, err := c.Collect(""); err == nil {
			t.Fatal("缺 sunxi_batchno 应报错")
		}
	})

	t.Run("sunxi_serial 非法", func(t *testing.T) {
		sys := copySysFixture(t, fakeSID, "xyz")
		c := &Collector{sysRoot: sys, eepromPath: "testdata/eeprom.bin"}
		_, err := c.Collect("")
		if err == nil {
			t.Fatal("非法 SID 应报错")
		}
		if strings.Contains(err.Error(), "xyz") {
			t.Errorf("错误文本不该回显 sunxi_serial 的值: %v", err)
		}
	})

	t.Run("无网卡", func(t *testing.T) {
		sys := copySysFixture(t, "", "")
		if err := os.RemoveAll(filepath.Join(sys, "class", "net")); err != nil {
			t.Fatal(err)
		}
		c := &Collector{sysRoot: sys, eepromPath: "testdata/eeprom.bin"}
		if _, err := c.Collect(""); err == nil {
			t.Fatal("读不到 wlan0 MAC 应报错")
		}
	})
}

// ---- 夹具工具 ----

func readFixtureEEPROM(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/eeprom.bin")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// copySysFixture 把 testdata/sys 复制到临时目录，并可选地替换 sunxi_info
// 里的一段文本（old 为空表示不替换）。
func copySysFixture(t *testing.T, old, replacement string) string {
	t.Helper()
	root := t.TempDir()
	src := "testdata/sys"
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if old != "" && strings.HasSuffix(p, "sys_info") {
			data = []byte(strings.Replace(string(data), old, replacement, 1))
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// growFirstRecord 把第一条记录的载荷尾部撑长 n 字节（0 填充），后续记录整体
// 后移，全部 CRC 重算。用来证明解析按记录头链走。
func growFirstRecord(t *testing.T, raw []byte, n int) []byte {
	t.Helper()
	size := int(binary.LittleEndian.Uint16(raw[eepromHeaderLen+4 : eepromHeaderLen+6]))
	end := eepromHeaderLen + recordHeaderLen + size

	out := make([]byte, 0, len(raw)+n)
	out = append(out, raw[:end-recordCRCLen]...) // 头 + 原载荷
	out = append(out, make([]byte, n)...)        // 撑长的 0 填充
	out = append(out, raw[end-recordCRCLen:]...) // 原 CRC + 后续记录
	out = out[:len(raw)]                         // 芯片容量不变，尾部截掉 0xff
	binary.LittleEndian.PutUint16(out[eepromHeaderLen+4:eepromHeaderLen+6], uint16(size+n))
	fixRecordCRCs(t, out)
	return out
}

// fixRecordCRCs 按记录头链重算每条记录的 CRC，让改过内容的夹具重新自洽。
func fixRecordCRCs(t *testing.T, raw []byte) {
	t.Helper()
	count := int(binary.LittleEndian.Uint16(raw[6:8]))
	off := eepromHeaderLen
	for range count {
		size := int(binary.LittleEndian.Uint16(raw[off+4 : off+6]))
		if off+recordHeaderLen+size > len(raw) {
			t.Fatalf("夹具记录越界（偏移 %d 长度 %d）", off, size)
		}
		hdr := raw[off : off+recordHeaderLen]
		payload := raw[off+recordHeaderLen : off+recordHeaderLen+size-recordCRCLen]
		binary.LittleEndian.PutUint16(raw[off+recordHeaderLen+size-recordCRCLen:], crc16ARC(hdr, payload))
		off += recordHeaderLen + size
	}
}
