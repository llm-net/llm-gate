// Command modeldata 从公开数据仓库 github.com/llm-net/llm-model-data 的正式发布
// （远端 `data-YYYY.MM.DD.N` tag）合成设备消费的模型目录文件 model-catalog.json。
// 映射与换算规则在 internal/modeldata；主仓用 `make -C firmware catalog` 一并完成
// 合成、校验与复制。
//
// 用法：
//
//	modeldata -repo <数据仓库检出> -out <model-catalog.json> [-ref data-2026.09.17.2] [-fetch] [-usd-cny 6.75]
//
// 缺省取本地已知的最新正式 tag；-fetch 先从 origin 拉取 tag。报告（未映射的产品、
// 没落地的模型、丢弃的计费分量、未定价的条目）打印到 stdout，一行一条。
// 退出码：0 成功；1 合成失败；2 用法或读仓库错误。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/llm-net/llm-gate/firmware/internal/modeldata"
)

func main() {
	repoDir := flag.String("repo", "", "数据仓库的本地检出目录")
	out := flag.String("out", "", "合成文件的输出路径")
	ref := flag.String("ref", "", "指定正式 tag（缺省取最新）")
	fetch := flag.Bool("fetch", false, "先 git fetch --tags origin")
	usdCNY := flag.String("usd-cny", modeldata.DefaultUSDCNY, "美元折人民币的固定汇率")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "用法：modeldata -repo <数据仓库检出> -out <model-catalog.json> [-ref <tag>] [-fetch] [-usd-cny <汇率>]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *repoDir == "" || *out == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	repo := modeldata.Repo{Dir: *repoDir}
	if *fetch {
		if err := repo.Fetch(); err != nil {
			fmt.Fprintln(os.Stderr, "modeldata:", err)
			os.Exit(2)
		}
	}
	tagName := *ref
	if tagName == "" {
		tag, err := repo.LatestTag()
		if err != nil {
			fmt.Fprintln(os.Stderr, "modeldata:", err)
			os.Exit(2)
		}
		tagName = tag.Name
	}
	in, err := modeldata.Load(repo, tagName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "modeldata:", err)
		os.Exit(2)
	}
	res, err := modeldata.Build(in, modeldata.Options{USDCNY: *usdCNY})
	if err != nil {
		fmt.Fprintln(os.Stderr, "modeldata:", err)
		os.Exit(1)
	}
	for _, line := range res.Report {
		fmt.Println("提示 " + line)
	}
	if err := os.WriteFile(*out, res.Raw, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "modeldata:", err)
		os.Exit(1)
	}
	fmt.Printf("modeldata: %s ← %s@%s（%d 字节）\n", *out, tagName, in.Commit[:12], len(res.Raw))
}
