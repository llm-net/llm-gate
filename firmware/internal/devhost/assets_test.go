package devhost

// 内嵌守护进程制品的校验：没有制品（没跑 make devd-assets 的裸 go test）即 skip，
// 有制品时两平台都得解得开、是 ELF、版本文件非空。

import (
	"bytes"
	"testing"
)

func TestEmbeddedBinaries(t *testing.T) {
	eb := &EmbeddedBinaries{}
	if eb.Version() == "" {
		t.Skip("没有内嵌守护进程制品（先执行 make devd-assets）")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		data, version, err := eb.Binary(arch)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if version != eb.Version() || len(data) < 1<<20 || !bytes.HasPrefix(data, []byte("\x7fELF")) {
			t.Fatalf("%s: 制品不成形状（%d 字节，版本 %q）", arch, len(data), version)
		}
		// 第二次走缓存，仍是同一份字节。
		again, _, _ := eb.Binary(arch)
		if !bytes.Equal(data, again) {
			t.Fatalf("%s: 缓存返回了不同的字节", arch)
		}
	}
	if _, _, err := eb.Binary("mips"); err == nil {
		t.Fatal("不支持的架构应报错")
	}
}
