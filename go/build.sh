#!/bin/sh
# 交叉编译打包 dsh-gate。
#
# 产物落在仓库根的 dist/<版本号>/ 下：每个平台一个目录，里面是那个平台的可执行
# 文件加 LICENSE 与 README；再打成 tar.gz（Windows 用 zip），并生成 SHA256SUMS。
#
# 用法：
#   sh go/build.sh              # 编译全部目标
#   sh go/build.sh linux/arm64  # 只编译指定目标
#
# 为什么值得有个脚本：交叉编译本身只要一行 go build，但「版本号从哪来」「打几个
# 包」「校验和怎么生成」这些琐事每次都重做一遍，迟早会漏。放进脚本就不会。
#
# 注意：dsh-gate 是**纯 Go**，CGO 必须关掉。这样二进制不依赖目标机的 libc 与
# CGO 运行时，`scp` 过去就能跑；代价是跨平台构建无需任何工具链。
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

# 版本号只有一个真源：internal/version/version.go 里的 Version。
# （它必须是变量而不是常量，否则 -ldflags -X 覆盖不了。）
version=$(sed -n 's/^var Version = "\(.*\)"$/\1/p' go/internal/version/version.go)
if [ -z "$version" ]; then
	echo "build.sh: 没能从 go/internal/version/version.go 里读出版本号" >&2
	exit 1
fi

# 用绝对路径：后面要进 go/ 目录里跑 go build，相对路径会在那里被解析到别处，
# 二进制就会静悄悄地落到 go/dist 下——这种错不报错，只是包是空的。
outdir="$root/dist/$version"

# 全部目标：平台/架构。armv7 单独一档，因为树莓派这类无界面服务器很常见。
all_targets="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 linux/arm windows/amd64 windows/arm64"

targets=${*:-$all_targets}

rm -rf "$outdir"
mkdir -p "$outdir"

# 先跑测试再编译。-count=1 不是多余的：Go 的测试缓存会跟踪「测试打开过的文件」，
# 而 README.md 在模块目录之外，改了它缓存也不会失效——文档被写坏时测试照样报通过
# （这个坑真的踩过：整份中文 README 被脚本换成了英文版，测试一片绿）。
echo "运行测试 ..."
(cd go && go test -count=1 ./...)

for target in $targets; do
	goos=${target%%/*}
	goarch=${target##*/}
	stage="$outdir/dsh-gate_${version}_${goos}_${goarch}"
	mkdir -p "$stage"

	binary=dsh-gate
	case $goos in
	windows) binary=dsh-gate.exe ;;
	esac

	echo "编译 $target ..."
	# 在 go/ 下构建：模块根是 go/，不是仓库根。
	(cd go && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch GOARM=${GOARM:-7} \
		go build -trimpath \
		-ldflags "-s -w -X github.com/wycto/dsh-gate/internal/version.Version=$version" \
		-o "$stage/$binary" ./cmd/dsh-gate)

	# 构建成功但没产出文件的情况必须当场失败：一个只有 README 没有二进制的包，
	# 下载的人要到解压之后才发现，比这里报错糟糕得多。
	if [ ! -f "$stage/$binary" ]; then
		echo "build.sh: $target 没有产出 $stage/$binary" >&2
		exit 1
	fi

	# 每个包里都带上这三份：分发二进制时最容易被漏掉、又最该一起给出去的东西。
	# QUICKSTART 是一页纸的「解压之后怎么跑起来」，README 是完整说明，LICENSE 是授权。
	cp LICENSE README.md QUICKSTART.md "$stage/"

	# README 里的截图是相对路径（docs/images/*.png）。不连图一起打包的话，
	# 解压出来看 README 全是裂图——所以按原目录结构把图带上。
	if [ -d docs/images ]; then
		mkdir -p "$stage/docs/images"
		cp docs/images/*.png "$stage/docs/images/"
	fi

	if [ "$goos" = windows ]; then
		(cd "$outdir" && zip -q -r "dsh-gate_${version}_${goos}_${goarch}.zip" "dsh-gate_${version}_${goos}_${goarch}")
	else
		(cd "$outdir" && tar -czf "dsh-gate_${version}_${goos}_${goarch}.tar.gz" "dsh-gate_${version}_${goos}_${goarch}")
	fi
done

# 校验和：让人下载后能验证包没被中间人换掉。
(cd "$outdir" && shasum -a 256 ./*.tar.gz ./*.zip 2>/dev/null >SHA256SUMS || true)

echo
echo "产物在 ${outdir}："
ls -1 "$outdir"