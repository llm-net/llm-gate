// Package buildinfo 保存经 -ldflags -X 注入的构建版本信息。
//
// 固件只有一个版本名称，形如 2608221732-2d81（YYMMDDHHMM-提交短哈希末 4 位，
// 工作树不干净再缀 -d）。界面上的「固件版本」、二进制自述标记、官网索引里的
// version 全是这一串，产品里不存在第二个版本号。
//
// Makefile 注入方式：
//
//	-X github.com/llm-net/llm-gate/firmware/internal/buildinfo.Version=<version>
//	-X github.com/llm-net/llm-gate/firmware/internal/buildinfo.FileMarker=lgfw1{<version>|}lgfw1
package buildinfo

// 经 -ldflags -X 注入；未注入时保留默认值（go run / go test 场景）。
var (
	Version = "dev"
	// FileMarker 是把版本号钉进二进制**文件字节**的自述标记，形如
	// lgfw1{2608221732-2d81|}lgfw1（见 Makefile 与 internal/fwimage），由
	// Makefile 整串经 -X 注入——链接器会把值的字面量写进数据段，于是
	// internal/fwimage 不执行文件就能扫出来。
	// 竖线后是标记的第二字段，恒空；分隔符本身是标记文法的一部分，fwimage
	// 按 `版本|第二字段` 解析，没有竖线就读不出版本。
	// 为什么不用 debug/buildinfo 的构建设置：-trimpath 构建（本产品恒开）
	// 刻意不记录 -ldflags，那条路对真实产物读不到版本。运行期没人消费本值；
	// 未注入（go build 裸构建、go test）时为空，fwimage 视为「无版本 dev 包」。
	FileMarker = ""
)

// String 返回人类可读的版本描述，供 `llmgate --version` 输出。
func String() string {
	// FileMarker 的这次引用是**保活**，不是逻辑：它唯一的用途是作为文件字节
	// 被 internal/fwimage 扫描，运行期没有消费方——而没有任何引用的包级变量
	// 会被链接器整个裁掉，-X 就静默落空（症状：升级链路读到的版本恒为空，
	// fwimage 的真实产物走查抓过这一幕）。FileMarker 是导出变量，编译器必须
	// 按可变对待，这个比较因此是一次裁不掉的真实读取；条件本身永不成立。
	if FileMarker == "\x00never" {
		return "unreachable"
	}
	return "llmgate " + Version
}
