// Command catalogcheck 校验目录数据的维护源：official-pricing.json（官方目录价）
// 与 platform-models.json（平台模型信息）。两份文件在被复制到官网与固件内嵌
// 副本之前必须通过这里（主仓用 `make -C firmware catalog` 一并完成校验与复制）。
//
// 用法：
//
//	catalogcheck [-fix] <目录>      校验目录里的两份文件；-fix 先把它们重排成规范格式
//	catalogcheck -fix <文件.json>   只重排一份文件
//
// 退出码：0 通过；1 有错误（逐条打印，前缀是文件名）；2 用法或读文件错误。
// 提示（不阻塞发布的信息）打印到 stdout，错误打印到 stderr。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/llm-net/llm-gate/firmware/internal/catalogcheck"
)

func main() {
	fix := flag.Bool("fix", false, "先把文件重排成规范格式（2 空格缩进、末尾换行）再校验")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法：catalogcheck [-fix] <目录>   或   catalogcheck -fix <文件.json>")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := flag.Arg(0)
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalogcheck:", err)
		os.Exit(2)
	}
	if !info.IsDir() {
		if !*fix {
			fmt.Fprintln(os.Stderr, "catalogcheck: 校验要给目录；单个文件只支持 -fix 重排")
			os.Exit(2)
		}
		if err := formatFile(target); err != nil {
			fmt.Fprintln(os.Stderr, "catalogcheck:", err)
			os.Exit(2)
		}
		return
	}
	if *fix {
		for _, name := range []string{catalogcheck.OfficialPricingFile, catalogcheck.PlatformModelsFile} {
			if err := formatFile(filepath.Join(target, name)); err != nil {
				fmt.Fprintln(os.Stderr, "catalogcheck:", err)
				os.Exit(2)
			}
		}
	}
	report, err := catalogcheck.CheckDir(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalogcheck:", err)
		os.Exit(2)
	}
	for _, line := range report.Infos {
		fmt.Println("提示 " + line)
	}
	if !report.OK() {
		for _, line := range report.Errors {
			fmt.Fprintln(os.Stderr, "错误 "+line)
		}
		fmt.Fprintf(os.Stderr, "catalogcheck: %d 处错误\n", len(report.Errors))
		os.Exit(1)
	}
	fmt.Printf("catalogcheck: %s 通过\n", strings.TrimSuffix(target, "/"))
}

// formatFile 就地重排一份文件；内容已是规范格式时不写。
func formatFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	formatted, err := catalogcheck.Format(raw)
	if err != nil {
		return fmt.Errorf("%s 不是合法 JSON：%v", filepath.Base(path), err)
	}
	if bytes.Equal(formatted, raw) {
		return nil
	}
	if err := os.WriteFile(path, formatted, 0o644); err != nil {
		return err
	}
	fmt.Printf("catalogcheck: 已重排 %s\n", path)
	return nil
}
