package boardinfo

// Radxa 板卡 EEPROM（"RADX" 容器）解析。
//
// 布局按 2026-08-07 在 cubie-a7a-dev 上实测（/sys/bus/i2c/devices/0-0050/eeprom，
// 2048 字节芯片，0x0100 之后整片 0xff），全部为小端：
//
//	文件头 12 字节： "RADX" | 版本 u16 | 记录数 u16 | 总长 u16 | 保留 u16
//	记录头  8 字节： id u16 | 类型 u16 | 长度 u16 | 标志 u16
//	记录体 长度 字节：载荷（长度−2）| CRC16 2 字节
//	下一条记录偏移 = 本记录头偏移 + 8 + 长度
//
// **按记录头走，不按死偏移**：本板的厂商/SKU/版本/序列号恰好落在
// 0x2a/0x4a/0x76/0x8a，但那是这一版内容的结果而非格式的承诺——换一版
// 内容长度就全变。唯一的定位手段是上面那条「+8+长度」的链。
//
// CRC 是 CRC-16/ARC（poly 0x8005 反射为 0xA001，init 0x0000，输入输出均
// 反射，无 xorout），覆盖「记录头 8 字节 || 载荷」，小端存在记录体末尾。
// 三条记录实测全部命中，因此本解析器**校验** CRC：EEPROM 是身份数据的源头，
// 读到坏块要报错，不能把半截值当真（决策 1「必填源读不到就报错」）。
//
// 实测本板三条记录：
//
//	type=1 产品记录：载荷里两段可打印串 = 厂商、SKU（其余为 0 填充）
//	type=2 PCB 版本：载荷 = 编码 u16 | NUL 结尾 ASCII
//	type=3 板卡序列号：同 type=2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// eepromMagic 是容器魔数。
const eepromMagic = "RADX"

// EEPROM 结构常量。
const (
	eepromHeaderLen = 12 // 文件头字节数
	recordHeaderLen = 8  // 记录头字节数
	recordCRCLen    = 2  // 记录体末尾的 CRC16
)

// 记录类型。
const (
	recTypeProduct     uint16 = 1 // 厂商 + SKU
	recTypePCBVersion  uint16 = 2 // PCB 版本
	recTypeBoardSerial uint16 = 3 // 板卡序列号
)

// ErrEEPROMFormat 表示 EEPROM 内容不是可识别的 RADX 容器（魔数不符、
// 记录越界、CRC 不匹配等）。
var ErrEEPROMFormat = errors.New("板卡 EEPROM 内容不可识别")

// eepromIdentity 是 EEPROM 提供的四项板卡身份。
type eepromIdentity struct {
	vendor      string
	sku         string
	pcbVersion  string
	boardSerial string
}

// eepromRecord 是一条解析出的记录。
type eepromRecord struct {
	id      uint16
	typ     uint16
	payload []byte
}

// parseEEPROM 按记录头链走完整个容器，返回全部记录。
func parseEEPROM(data []byte) ([]eepromRecord, error) {
	if len(data) < eepromHeaderLen || string(data[:4]) != eepromMagic {
		return nil, fmt.Errorf("%w：缺少 %s 魔数", ErrEEPROMFormat, eepromMagic)
	}
	version := binary.LittleEndian.Uint16(data[4:6])
	if version != 1 {
		return nil, fmt.Errorf("%w：不支持的容器版本 %d", ErrEEPROMFormat, version)
	}
	count := int(binary.LittleEndian.Uint16(data[6:8]))
	if count == 0 {
		return nil, fmt.Errorf("%w：记录数为 0", ErrEEPROMFormat)
	}

	records := make([]eepromRecord, 0, count)
	off := eepromHeaderLen
	for i := range count {
		if off+recordHeaderLen > len(data) {
			return nil, fmt.Errorf("%w：第 %d 条记录头越界（偏移 %d）", ErrEEPROMFormat, i+1, off)
		}
		hdr := data[off : off+recordHeaderLen]
		size := int(binary.LittleEndian.Uint16(hdr[4:6]))
		if size < recordCRCLen || off+recordHeaderLen+size > len(data) {
			return nil, fmt.Errorf("%w：第 %d 条记录长度 %d 越界（偏移 %d）", ErrEEPROMFormat, i+1, size, off)
		}
		body := data[off+recordHeaderLen : off+recordHeaderLen+size]
		payload := body[:size-recordCRCLen]
		want := binary.LittleEndian.Uint16(body[size-recordCRCLen:])
		if got := crc16ARC(hdr, payload); got != want {
			return nil, fmt.Errorf("%w：第 %d 条记录 CRC 校验失败（偏移 %d）", ErrEEPROMFormat, i+1, off)
		}
		records = append(records, eepromRecord{
			id:      binary.LittleEndian.Uint16(hdr[0:2]),
			typ:     binary.LittleEndian.Uint16(hdr[2:4]),
			payload: payload,
		})
		off += recordHeaderLen + size
	}
	return records, nil
}

// readEEPROMIdentity 从记录表里取四项身份。任一项缺失即报错——身份数据
// 与 sysinfo 的容错口径相反，读不到就报错，不静默省略（决策 1）。
func readEEPROMIdentity(data []byte) (eepromIdentity, error) {
	records, err := parseEEPROM(data)
	if err != nil {
		return eepromIdentity{}, err
	}

	var id eepromIdentity
	for _, rec := range records {
		switch rec.typ {
		case recTypeProduct:
			// 产品记录里厂商与 SKU 是两段以 NUL 分隔的可打印串。要求恰好
			// 两段：多一段少一段都说明格式与实测不符，宁可报错也不猜哪段
			// 是哪个字段。
			runs := printableRuns(rec.payload)
			if len(runs) != 2 {
				return eepromIdentity{}, fmt.Errorf(
					"%w：产品记录含 %d 段文本，预期 2 段（厂商、SKU）", ErrEEPROMFormat, len(runs))
			}
			id.vendor, id.sku = runs[0], runs[1]
		case recTypePCBVersion:
			id.pcbVersion = recordText(rec.payload)
		case recTypeBoardSerial:
			id.boardSerial = recordText(rec.payload)
		}
	}

	for _, f := range []struct {
		name  string
		value string
	}{
		{"厂商", id.vendor},
		{"SKU", id.sku},
		{"PCB 版本", id.pcbVersion},
		{"板卡序列号", id.boardSerial},
	} {
		if f.value == "" {
			return eepromIdentity{}, fmt.Errorf("%w：缺少%s记录", ErrEEPROMFormat, f.name)
		}
	}
	return id, nil
}

// recordText 取一条「编码 u16 | NUL 结尾 ASCII」记录的文本。
func recordText(payload []byte) string {
	if len(payload) < 2 {
		return ""
	}
	runs := printableRuns(payload[2:])
	if len(runs) == 0 {
		return ""
	}
	return runs[0]
}

// printableRuns 切出载荷里所有以非可打印字节（0 填充）分隔的文本段，
// 顺序保留，两端空白已裁掉（实测厂商串是右对齐的「  Radxa Computer」），
// 裁完为空的段丢弃。
func printableRuns(payload []byte) []string {
	var out []string
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if s := strings.TrimSpace(string(payload[start:end])); s != "" {
			out = append(out, s)
		}
		start = -1
	}
	for i, b := range payload {
		if b >= 0x20 && b <= 0x7e {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(payload))
	return out
}

// crc16ARC 计算 CRC-16/ARC（poly 0x8005 反射为 0xA001，init 0x0000，
// 输入输出均反射，无 xorout），依次覆盖各段。
func crc16ARC(parts ...[]byte) uint16 {
	crc := uint16(0)
	for _, p := range parts {
		for _, b := range p {
			crc ^= uint16(b)
			for range 8 {
				if crc&1 != 0 {
					crc = crc>>1 ^ 0xA001
				} else {
					crc >>= 1
				}
			}
		}
	}
	return crc
}
