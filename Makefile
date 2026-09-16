BINARY  := quickshare
export CGO_ENABLED := 0

# ---- 版本信息 ----
#
# 全部从 git 推出来，**不在源码里写死**：写死的版本号迟早和实际编出来的代码对不上，
# 而用户报问题时报的就是它。没有 git（或仓库还没提交过）时退回 dev，
# 于是任何环境下 `make` 都能跑通。
#
# VERSION 用 git describe：停在 tag 上就是 tag 名，tag 之后又提交过就是
# `1.2.3-4-gabc1234` 这种形式。它不是合法 semver，version.Compare 会判成
# "比不出来"从而不提示更新——**本地构建正是想要这个行为**。
GIT_DESC := $(shell git describe --tags --always --dirty 2>/dev/null)
VERSION  ?= $(if $(GIT_DESC),$(patsubst v%,%,$(GIT_DESC)),dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# 从 origin 抠出 owner/name，更新检查要用。抠不出来就留空 —— **空 = 不做更新检查**，
# 这正是本地构建该有的行为。fork 之后自己发布的话，直接 `make REPO=你/仓库名`。
#
# -E 不能省：`+` 和 `()` 在基本正则里是**字面字符**，漏了它 sed 会直接报
# "invalid reference \1"，输出空串。那种失败很安静——版本号照常注入，
# 只是更新检查悄悄没了，排查时容易怀疑到别处去。
# 两种远程写法都认：https://github.com/a/b.git 和 git@github.com:a/b.git
REPO ?= $(shell git config --get remote.origin.url 2>/dev/null \
          | sed -e 's/[.]git$$//' -E -e 's#.*[/:]([^/]+/[^/]+)$$#\1#')

LDFLAGS := -s -w \
  -X quickshare/internal/version.Version=$(VERSION) \
  -X quickshare/internal/version.Commit=$(COMMIT) \
  -X quickshare/internal/version.Date=$(DATE) \
  -X quickshare/internal/version.Repo=$(REPO)

GOFLAGS_BUILD := -trimpath -ldflags="$(LDFLAGS)"

# 当前平台的可执行文件后缀（Windows 需要 .exe，Linux/macOS 不需要）
GOOS_NOW := $(shell go env GOOS)
ifeq ($(GOOS_NOW),windows)
EXE := .exe
endif

.PHONY: build run dist windows linux-amd64 linux-arm64 linux-arm version image tidy clean

# 编译当前平台，产物统一放 dist/，不污染项目根目录
build:
	go build $(GOFLAGS_BUILD) -o dist/$(BINARY)$(EXE) .

run:
	go run . -addr :8080 -data ./data

# ---- NAS 目标平台（产物是 Linux ELF，没有扩展名）----

windows:
	GOOS=windows GOARCH=amd64 go build $(GOFLAGS_BUILD) -o dist/$(BINARY)-windows-amd64.exe .

linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS_BUILD) -o dist/$(BINARY)-linux-amd64 .

linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS_BUILD) -o dist/$(BINARY)-linux-arm64 .

linux-arm:
	GOOS=linux GOARCH=arm GOARM=7 go build $(GOFLAGS_BUILD) -o dist/$(BINARY)-linux-armv7 .

# 一次编出所有 NAS 架构
dist: linux-amd64 linux-arm64 linux-arm

# 看看这次构建会注入什么版本（排查"界面上的版本号不对"时先跑这个）
version:
	@echo "VERSION = $(VERSION)"
	@echo "COMMIT  = $(COMMIT)"
	@echo "DATE    = $(DATE)"
	@echo "REPO    = $(REPO)"

# 本地构建镜像。和发布出去的那个**注的是同一组变量**——不传的话镜像里的版本就是 dev，
# 界面上的「关于」会显示"这个构建没有配置更新检查"，那是刻意的（自己构建的二进制不该
# 假装成某个正式版本）。要带上版本号：`make image VERSION=1.0.0 REPO=你/仓库名`
#
# 只构建本机架构；发布用的三架构由 .github/workflows/docker.yml 用 buildx 出。
image:
	docker build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(DATE) \
	  --build-arg REPO=$(REPO) \
	  -t $(BINARY):$(VERSION) -t $(BINARY):latest .

tidy:
	go mod tidy

clean:
	rm -rf dist $(BINARY) $(BINARY).exe
