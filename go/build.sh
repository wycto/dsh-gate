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
# 约定：**每次打包测试都要把 version.go 的小版本号顺延一位**（补丁号 +1），让测试包
# 与上一次的产物可区分、不互相覆盖；本脚本不自动改版本号——版本号是要提交进仓库的
# 事实，悄悄改会让工作区和提交对不上。正式发版另走发版流程（升版本 + CHANGELOG）。
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

# ---- macOS 元数据污染防护 ----------------------------------------------------
# 在 macOS 上打 tar 时，bsdtar 会把文件的扩展属性（com.apple.quarantine /
# com.apple.provenance）写成 PAX 头 `LIBARCHIVE.xattr.*` / `SCHILY.xattr.*`，
# 并且给每个条目额外塞一个 `._名字` 的 AppleDouble 文件。后果不是「难看」：
#   * 用户解压后目录里多出一堆 163 字节的 ._ 文件与 .DS_Store；
#   * GNU tar / 7-Zip 解这些包时会刷一屏
#     "Ignoring unknown extended header keyword 'LIBARCHIVE.xattr.com.apple.quarantine'"，
#     看起来就像包坏了；
#   * 压缩包体积无谓变大（0.3.14 的 linux/amd64 包里 30 个条目有 15 个是 ._）。
# COPYFILE_DISABLE=1 只挡住 AppleDouble，挡不住 PAX 里的 xattr 关键字，所以两者都要。
# 这两个开关都是 bsdtar 专有的：GNU tar 不认识 --no-mac-metadata，在老 Linux 上
# 构建会直接失败，因此先探测再决定加不加——不能假定构建机一定是 macOS。
TAR_META_FLAGS=""
if tar --no-mac-metadata -cf /dev/null --files-from /dev/null 2>/dev/null; then
	TAR_META_FLAGS="$TAR_META_FLAGS --no-mac-metadata"
fi
if tar --no-xattrs -cf /dev/null --files-from /dev/null 2>/dev/null; then
	TAR_META_FLAGS="$TAR_META_FLAGS --no-xattrs"
fi

# assert_no_mac_junk：打完包当场验一遍，防止上面任何一层失效后把脏包发出去。
#
# 不能靠 `tar -tzf` 判断：macOS 自己的 bsdtar 列出包内容时会把 `._xxx` 这类
# AppleDouble 条目**从清单里藏掉**（它把 AppleDouble 合并回扩展属性来展示），
# 所以 `tar -t` 看着干净、Linux 上解开却满目录 `._`。这里改成扫 tar 的原始字节流，
# 并且必须**按记录边界精确匹配**：
#   * 不能直接 grep "SCHILY.xattr."——Go 二进制的字符串池里就有这个字面量，
#     干净包会被判成脏包（真踩过）。PAX 扩展头是一条以「长度 关键字=值」开头的
#     记录，锚定行首的十进制长度才不会误伤。
#   * AppleDouble 的条目名按规则是 `<目录>/._<原文件名>`，所以拿暂存目录里的
#     真实条目名逐个比对，同样锚定整行（NUL 分隔后正好一行一个名字）。
assert_no_mac_junk() {
	pkg=$1
	stage=$2
	raw="$outdir/.check.raw"
	lines="$outdir/.check.lines"

	gzip -dc "$pkg" >"$raw"
	# tar 的记录名与 PAX 头都是 NUL 分隔/结尾的，转成换行后每条记录独占一行。
	LC_ALL=C tr '\000' '\n' <"$raw" >"$lines"

	bad=""
	if LC_ALL=C grep -aqE '^[0-9]{1,6} (LIBARCHIVE|SCHILY)\.(xattr|acl)\.' "$lines"; then
		bad="pax"
	fi

	# 包内路径带一层顶层目录（tar 打的是 dsh-gate_<版本>_<系统>_<架构>/ 本身）。
	prefix=$(basename "$stage")
	for rel in $(cd "$stage" && find . -mindepth 1 | sed 's|^\./||'); do
		base=${rel##*/}
		dir=${rel%/*}
		if [ "$dir" = "$rel" ]; then
			expect="$prefix/._$base"
		else
			expect="$prefix/$dir/._$base"
		fi
		if LC_ALL=C grep -axqF "$expect" "$lines"; then
			bad="${bad}${bad:+,}$rel"
		fi
	done
	rm -f "$raw" "$lines"

	if [ -n "$bad" ]; then
		echo "build.sh: $pkg 里混进了 macOS 元数据，拒绝产出这个包。" >&2
		echo "  受影响条目（最多列 5 个）: $(printf '%s' "$bad" | tr ',' '\n' | head -5 | tr '\n' ' ')" >&2
		echo "  后果: Linux 用户解压后满目录 ._ 文件，GNU tar / 7-Zip 还会刷" >&2
		echo "        'Ignoring unknown extended header keyword'，看起来像包坏了。" >&2
		exit 1
	fi
}

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

	# macOS 自带的 bsdtar / zip 会把扩展属性（quarantine、provenance）和每个
	# 文件的 AppleDouble 副本（._xxx）一起写进压缩包：在 Linux 上解压后，包里
	# 会多出一堆 163/213 字节的 ._ 文件，看起来像「解压坏了」。
	#   * COPYFILE_DISABLE=1 让 bsdtar 根本不生成 AppleDouble；
	#   * --exclude 兜住已经生成的 ._ 条目与 .DS_Store（不依赖 libarchive 版本）；
	#   * --no-mac-metadata / --no-xattrs 再挡住 PAX 头里的 xattr 关键字 ——
	#     只挡 AppleDouble 的话，GNU tar 仍会刷一屏 "Ignoring unknown extended
	#     header keyword"，7-Zip 仍报 Headers Error（用 0.3.14 实测过）。
	# 后两个开关是 bsdtar 专有的，老 GNU tar 不认识，所以先探测再拼进变量。
	pkg=
	case $goos in
	windows)
		pkg="dsh-gate_${version}_${goos}_${goarch}.zip"
		(cd "$outdir" && zip -q -r -X "$pkg" \
			"dsh-gate_${version}_${goos}_${goarch}" -x '*/._*')
		# zip 一般不会带 AppleDouble，但既然 tar 那侧出过事，这边也顺手验一眼。
		if command -v unzip >/dev/null 2>&1; then
			if unzip -Z1 "$outdir/$pkg" | grep -qE '(^|/)\._'; then
				echo "build.sh: $pkg 里混进了 macOS 元数据文件，拒绝产出这个包。" >&2
				exit 1
			fi
		fi
		;;
	*)
		pkg="dsh-gate_${version}_${goos}_${goarch}.tar.gz"
		(cd "$outdir" && COPYFILE_DISABLE=1 tar \
			--exclude '._*' --exclude '.DS_Store' \
			$TAR_META_FLAGS \
			-czf "$pkg" "dsh-gate_${version}_${goos}_${goarch}")
		assert_no_mac_junk "$outdir/$pkg" "$stage"
		;;
	esac
done

# 校验和：让人下载后能验证包没被中间人换掉。
# macOS 只有 shasum，大多数 Linux 只有 sha256sum：先探测再用，别让缺命令时
# 静悄悄地留下一个空的 SHA256SUMS（原来的 `|| true` 正好会掩盖这种失败）。
sha_cmd=
if command -v sha256sum >/dev/null 2>&1; then
	sha_cmd=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	sha_cmd="shasum -a 256"
fi
if [ -n "$sha_cmd" ]; then
	(
		cd "$outdir"
		for f in ./*.tar.gz ./*.zip; do
			# 没匹配到的通配符会原样留在变量里（例如只打 Linux 包时是 ./*.zip）。
			# 不能写成 `[ -f "$f" ] && $sha_cmd "$f"`：最后一轮判定为假时，
			# 这个列表的退出码就是 1，set -e 会让脚本在产品齐全后仍以 1 退出
			# （构建产物都在、却像失败，单平台打包时踩过）。
			[ -f "$f" ] || continue
			$sha_cmd "$f"
		done
	) >"$outdir/SHA256SUMS"
else
	echo "build.sh: 找不到 sha256sum / shasum，跳过 SHA256SUMS" >&2
fi

echo
echo "产物在 ${outdir}："
ls -1 "$outdir"