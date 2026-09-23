# llmgate — 单一 multi-call Go 二进制（架构决策 29）。
# 硬约束：CGO_ENABLED=0，产物必须是静态链接单二进制。交付平台两个：linux/arm64（板子与
# ARM64 主机）与 linux/amd64（x86-64 云主机与电脑），同一 VERSION 一次发布两份制品。

MODULE  := github.com/llm-net/llm-gate/firmware
BIN_DIR := bin

# 固件只有一个版本名称：`YYMMDDHHMM-<提交短哈希末 4 位>`，工作树不干净再缀 `-d`
# （2608221732-2d81 / 2608221629-1de9-d）。产品里没有第二个版本号——界面上的
# 「固件版本」、自述标记、官网索引里的 version 全是这一串。
#
# 十位时间戳精确到分钟，取构建时刻的本地时区，天然单调递增，官网索引的版本比较
# （internal/officialsite.compareVersion）只看它；末 4 位哈希是给人分辨同一
# 分钟里不同提交的，不参与比较。没有 git（源码包构建）时哈希位退化成 0000。
#
# 时间戳精确到分钟，是因为同一时间戳的两个包互不构成升级：只记到小时的话，
# 一小时内的第二次构建版本串不变，gate update 与固件升级都判成「不比当前新」，
# 迭代节奏比小时快时升级链路就整个哑掉。
BUILD_STAMP := $(shell date +%y%m%d%H%M)
GIT_SUFFIX  := $(shell git rev-parse --short=7 HEAD 2>/dev/null | tail -c 5)
GIT_DIRTY   := $(shell git status --porcelain 2>/dev/null | head -n 1)
VERSION ?= $(BUILD_STAMP)-$(or $(GIT_SUFFIX),0000)$(if $(GIT_DIRTY),-d)

# FileMarker 把版本钉进二进制文件字节（lgfw1{版本|}lgfw1），供升级链路
# 不执行文件即读出版本（internal/fwimage；-trimpath 下 debug/buildinfo 不记录
# -ldflags，所以需要这个自述标记）。两个 -X 的 VERSION 必须一致。
# 注意：整串 LDFLAGS 在配方里以 -ldflags '$(LDFLAGS)' 单引号包裹，FileMarker
# 里的 { | } 因此按字面进值——这里不能再套引号（嵌套引号会把配方劈断）。
#
# 竖线后那个空位是标记的第二字段，恒空；分隔符本身是标记文法的一部分，
# internal/fwimage 按 `版本|第二字段` 解析，去掉竖线就读不出版本。
LDFLAGS := -X $(MODULE)/internal/buildinfo.Version=$(VERSION) \
           -X $(MODULE)/internal/buildinfo.FileMarker=lgfw1{$(VERSION)|}lgfw1

.PHONY: build build-arm64 build-amd64 release release-arm64 release-amd64 release-check print-version gate-assets devd-assets web catalog check test clean

## print-version: 打印本次构建会用的版本名称。发布时先取这一串，再把同一个 VERSION 显式传给
## release——两次 make 之间分钟数可能翻页，只算一次才不会出现制品自述版本与索引 version 不一致。
print-version:
	@echo $(VERSION)

## gate-assets: 交叉构建六个平台的 gate 客户端并生成固件内嵌清单。
## 主仓不追踪 internal/gatehelper/assets/ 里的制品（只有 .gitkeep），每次 build/test 都由这里重建，
## 制品变化不会弄脏工作树，也不会把版本推成 -d。
## 公开源码仓库（github.com/llm-net/llm-gate）同样带 gate/ 源码、同样不追踪这些制品，
## 走的是与主仓一样的重建路径，只需要 Go 工具链。没有 ../gate 的源码包（例如只解开
## firmware/ 的分发包）才回退到已有制品，两者都没有时报错——与 web 目标同规矩。
gate-assets:
	@if [ -d ../gate ]; then \
	  $(MAKE) -C ../gate assets VERSION=$(VERSION) OUTPUT=../firmware/internal/gatehelper/assets; \
	elif [ -f internal/gatehelper/assets/stable.json ]; then \
	  echo "gate-assets: 没有 ../gate，使用已有的内嵌制品"; \
	else \
	  echo "gate-assets: 没有 ../gate 也没有 internal/gatehelper/assets 制品，无法内嵌 gate 客户端" >&2; exit 1; \
	fi

## devd-assets: 交叉构建 linux/amd64 与 linux/arm64 的两个主机守护进程——工作节点的 llmgate-devd
## （cmd/llmgate-devd）与模型服务节点的 llmgate-modeld（cmd/llmgate-modeld），都是纯标准库——并 gzip 成
## 固件内嵌制品 internal/devhost/assets/<名>-linux-<arch>.gz 与共用的 VERSION。
## 与 gate-assets 同规矩：目录只跟踪 .gitkeep，制品被 gitignore，每次 build/test 重建；固件把它按目标
## 架构经 SSH 送到纳管主机上，也给「手动安装」下载。版本随固件同一个 VERSION。
DEVD_ASSETS := internal/devhost/assets
DAEMONS := llmgate-devd llmgate-modeld
devd-assets:
	@mkdir -p $(BIN_DIR) $(DEVD_ASSETS)
	@for name in $(DAEMONS); do for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$$name-linux-$$arch ./cmd/$$name || exit 1; \
	  gzip -9 -n -c $(BIN_DIR)/$$name-linux-$$arch > $(DEVD_ASSETS)/$$name-linux-$$arch.gz || exit 1; \
	done; done
	@printf '%s\n' '$(VERSION)' > $(DEVD_ASSETS)/VERSION

## build: 本机构建 bin/llmgate
build: gate-assets devd-assets web
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/llmgate ./cmd/llmgate

## build-arm64: 交叉编译 linux/arm64 静态二进制（开发板与 ARM64 主机），产物 bin/llmgate-linux-arm64
build-arm64: gate-assets devd-assets web
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/llmgate-linux-arm64 ./cmd/llmgate

## build-amd64: 交叉编译 linux/amd64 静态二进制（x86-64 云主机与电脑），产物 bin/llmgate-linux-amd64
build-amd64: gate-assets devd-assets web
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/llmgate-linux-amd64 ./cmd/llmgate

## release-check: 正式发布的前置闸。拒绝脏工作树与不成形状的 VERSION——版本名称由构建时刻
## 与提交推出，不该手填；带 -d 的脏包更不能进官网索引。
release-check:
	@case "$(VERSION)" in \
	  [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; \
	  *) echo "VERSION 必须是 YYMMDDHHMM-xxxx 形状且不带 -d（当前：$(VERSION)）" >&2; exit 1 ;; \
	esac
	@if [ -n "$$(git status --porcelain 2>/dev/null)" ]; then \
	  echo "工作树不干净：正式发布必须从已提交的代码构建" >&2; exit 1; \
	fi

## release: 正式发布构建——同一个 VERSION 出 linux/arm64 与 linux/amd64 两份固件，并把两份的
## SHA-256 连同安装脚本及内嵌 gate 压缩包的条目写成 bin/SHA256SUMS（`<sha256>  <文件名>` 一行一个，
## install.sh 与 `sha256sum -c` 都按这个形状读）。Release 上传的就是 bin/ 里这四个文件加
## internal/gatehelper/assets/ 的六个压缩包。用法：make release
release: release-check
	$(MAKE) build-arm64 build-amd64 VERSION=$(VERSION)
	@if [ -f ../website/public/install.sh ]; then \
	  cp ../website/public/install.sh $(BIN_DIR)/install.sh; \
	elif [ -f ../install.sh ]; then \
	  cp ../install.sh $(BIN_DIR)/install.sh; \
	else \
	  echo "release: install.sh 不在场" >&2; exit 1; \
	fi
	@cd $(BIN_DIR) && sha256sum llmgate-linux-arm64 llmgate-linux-amd64 install.sh > SHA256SUMS
	@if [ -f internal/gatehelper/assets/SHA256SUMS ]; then \
	  cat internal/gatehelper/assets/SHA256SUMS >> $(BIN_DIR)/SHA256SUMS; \
	fi
	@cat $(BIN_DIR)/SHA256SUMS

## release-arm64 / release-amd64: 只出一个平台的正式构建（补发单平台制品时用；版本纪律同 release）。
release-arm64: release-check
	$(MAKE) build-arm64 VERSION=$(VERSION)
	@sha256sum $(BIN_DIR)/llmgate-linux-arm64

release-amd64: release-check
	$(MAKE) build-amd64 VERSION=$(VERSION)
	@sha256sum $(BIN_DIR)/llmgate-linux-amd64

## web: 重建设备界面产物 web/ui → internal/ui/uidist（挂在 /ui/，embed 进二进制）。
## 产物不入库（目录只跟踪 .gitkeep），build / test 每次先经这里重建，与 gate-assets 同规矩。
## 有 npm 就重建（node_modules 缺失时先 npm ci；WEB_INSTALL=1 强制重装）；没有 npm 但
## 已有产物（公开源码快照随带 uidist）就直接内嵌；两者都没有时报错。
web:
	@if command -v npm >/dev/null 2>&1; then \
	  cd web/ui && { [ -d node_modules ] && [ -z "$(WEB_INSTALL)" ] || npm ci; } && npm run build; \
	elif [ -f internal/ui/uidist/index.html ]; then \
	  echo "web: 没有 npm，使用已有的 internal/ui/uidist 产物"; \
	else \
	  echo "web: 没有 npm 也没有 internal/ui/uidist 产物，无法内嵌设备界面" >&2; exit 1; \
	fi

## catalog: 从公开数据仓库 llm-model-data 的最新正式 tag 合成模型目录文件
## model-catalog.json（tools/modeldata），校验（tools/catalogcheck）后写官网副本并复制到
## 固件内嵌副本。两份必须逐字节一致（internal/platformcatalog 与 internal/catalogcheck 的
## 绊线测试把守）；不在副本上直接改：数据改到数据仓库并打正式 tag，映射改到 internal/modeldata。
## 用法：make catalog [MODEL_DATA=<数据仓库检出>] [MODEL_DATA_REF=<tag>] [MODEL_DATA_FETCH=1]
MODEL_DATA ?= ../llm-model-data
MODEL_DATA_REF ?=
MODEL_DATA_FETCH ?=
CATALOG_OUT := ../website/public/updates/data/model-catalog.json
catalog:
	@if [ ! -d "$(MODEL_DATA)/.git" ]; then \
	  echo "catalog: $(MODEL_DATA) 不是数据仓库的 git 检出（git clone https://github.com/llm-net/llm-model-data.git）" >&2; exit 1; \
	fi
	go run ./tools/modeldata -repo "$(MODEL_DATA)" -out "$(CATALOG_OUT)" \
	  $(if $(MODEL_DATA_REF),-ref "$(MODEL_DATA_REF)") $(if $(MODEL_DATA_FETCH),-fetch)
	go run ./tools/catalogcheck "$(CATALOG_OUT)"
	cp "$(CATALOG_OUT)" internal/platformcatalog/model-catalog.json
	go test ./internal/platformcatalog/ ./internal/catalogcheck/ ./internal/modeldata/

## check: 静态检查
check:
	go vet ./...

## test: 全量测试
## test: 先重建 gate 内嵌制品与设备界面产物，再跑全部包测试（gatehelper 的制品校验与
## internal/ui 的产物校验没有制品时会 skip）。
test: gate-assets devd-assets web
	go test ./...

clean:
	rm -rf $(BIN_DIR)
