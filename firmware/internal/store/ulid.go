package store

import (
	"crypto/rand"
	"time"
)

// ulidAlphabet 是 Crockford base32：不含 I、L、O、U，大小写不敏感，肉眼与
// 口头传抄都不易混淆。
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewULID 生成一个 26 位 ULID：前 10 位编码 48 位毫秒时间戳，后 16 位编码
// 80 位 crypto/rand 随机数。同一毫秒内不保证单调递增，只保证不重复；用作
// 任务这类按创建时间排序、又要能当文件名的标识（字典序即时间序，无需转义）。
func NewULID(now time.Time) string {
	var entropy [10]byte
	rand.Read(entropy[:]) // 自 Go 1.24 起 crypto/rand.Read 保证不失败
	ms := uint64(now.UnixMilli())
	var out [26]byte
	for i := 9; i >= 0; i-- {
		out[i] = ulidAlphabet[ms&0x1f]
		ms >>= 5
	}
	// 80 位随机数按 5 位一组切成 16 个字符：把 10 字节当大端整数逐位取。
	var acc uint64
	bits := 0
	pos := 10
	for _, b := range entropy {
		acc = acc<<8 | uint64(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[pos] = ulidAlphabet[(acc>>uint(bits))&0x1f]
			pos++
		}
	}
	return string(out[:])
}

// ULIDTime 解出 ULID 里的毫秒时间戳；不是 26 位合法 ULID 时返回 false。
func ULIDTime(id string) (time.Time, bool) {
	if len(id) != 26 {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < 26; i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		v := -1
		for j := 0; j < len(ulidAlphabet); j++ {
			if ulidAlphabet[j] == c {
				v = j
				break
			}
		}
		if v < 0 {
			return time.Time{}, false
		}
		if i < 10 {
			ms = ms<<5 | uint64(v)
		}
	}
	return time.UnixMilli(int64(ms)), true
}
