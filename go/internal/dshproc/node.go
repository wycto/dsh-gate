package dshproc

// Node.js 运行时的自备：服务器上没有 Node.js 时，网关自己把官方二进制包
// 下载到状态目录里用，而不是把「你自己去装一个」丢给用户。
//
// 为什么是下载官方包而不是调系统包管理器：
//
//   - **不需要 root，也不改系统**。apt / dnf 装 Node 会动发行版的包状态，
//     而网关只是个应用；把它需要的运行时放进自己的状态目录，卸载时跟着
//     `uninstall --purge` 一起消失，边界干净。
//   - **版本可控**。发行版仓库里的 Node 往往很旧，而 dsh 的入口依赖
//     `import.meta.main`（实测 Node 20/22 没有、26 有，见 install.go 的记录），
//     所以这里取 nodejs.org 上的最新版，而不是 LTS。
//
// 安全上只有一条底线：**下载完必须对 nodejs.org 的 SHASUMS256.txt 校验**。
// 我们要执行的是刚下下来的二进制，校验和是从同一个 HTTPS 源取的——它挡不住
// 攻破 nodejs.org 的攻击者，但能挡住传输损坏、镜像不一致和「下到一半」。
//
// 复用规则：已经装好（<状态目录>/node/<版本-平台>/bin/npx 存在）就直接用，
// 不重复下载；系统 PATH 里本来就有 npx 时什么都不做。

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	// nodeDistBase 是官方分发包的根地址。
	nodeDistBase = "https://nodejs.org/dist"
	// nodeMirrorBase 是 Node 二进制的国内镜像（npmmirror 的 binary 镜像），
	// 官方源拉不动时退到它。它同样提供 index.json 与 SHASUMS256.txt，
	// 所以校验流程一个字都不用改。
	nodeMirrorBase = "https://registry.npmmirror.com/-/binary/node"
	// nodeDownloadTimeout 是单个请求的上限：40MB 的包在慢网络上也够用，
	// 同时不会让卡死的连接永远挂住启动流程。
	nodeDownloadTimeout = 15 * time.Minute
	// nodeSmokeTimeout 是「解出来的 npx 真的能跑吗」的验证上限。
	nodeSmokeTimeout = 2 * time.Minute
	// nodeDirName 是状态目录里存放运行时的那一层。
	nodeDirName = "node"
	// nodeVersionAttempts 是最多往下试几个版本。
	//
	// 取最新版时偶尔会撞上「这个平台当晚还没出包」（发行是分批的），往下试
	// 一两个就能拿到；但也不能翻遍整张表，那只会把启动拖成几分钟。
	nodeVersionAttempts = 4
)

// nodeSources 是按顺序尝试的下载源：官方优先，官方拉不动再退镜像。
var nodeSources = []string{nodeDistBase, nodeMirrorBase}

// nodeRelease 是 index.json 里的一条版本记录。
//
// 只用 version：同一个文件里的 files 字段是 osx-x64-tar 这种粗粒度名字，
// 不能直接对应官方包名，判断「这个平台有没有包」要靠下面 SHASUMS256 里的
// 真实文件名。
type nodeRelease struct {
	Version string `json:"version"`
	LTS     any    `json:"lts"`
}

// EnsureNodeRuntime 保证本机有一个可用的 npx，返回需要加进 PATH 的 bin 目录。
//
// 返回空串表示「系统 PATH 里已经有 npx，什么都不用做」。返回非空时，调用方
// 应当把该目录**前置**到 PATH：npx 的 shebang 是 `#!/usr/bin/env node`，
// 只给绝对路径、不把它所在的 bin 目录放进 PATH，子进程仍然找不到 node。
//
// 失败不返回半成品：任何一步出错都会把临时目录删掉，下一次启动重头再来。
func EnsureNodeRuntime(ctx context.Context, log *slog.Logger, stateDir string) (string, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	// 系统里已经有 npx 就别画蛇添足：用户装过的才是他想要的那个版本。
	if lookPathAny("npx", "npx.cmd") != "" {
		return "", nil
	}
	if binDir, ok := LocalNodeBin(stateDir); ok {
		log.Info("使用网关自备的 Node.js 运行时", "bin", binDir)
		return binDir, nil
	}
	if stateDir == "" {
		return "", errors.New("状态目录为空，无法存放自备的 Node.js 运行时")
	}
	if msg := nodePlatformBlocker(); msg != "" {
		return "", errors.New(msg)
	}

	platform, err := nodePlatformKey()
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: nodeDownloadTimeout}

	root := filepath.Join(stateDir, nodeDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %w", root, err)
	}

	var lastErr error
	// 先官方、后镜像：nodejs.org 在有些网络里慢到不可用，而这不是用户能
	// 自己解决的问题（他连 Node.js 都还没有）——所以这里也自己换源。
	for sourceIndex, base := range nodeSources {
		versions, err := nodeVersions(ctx, client, base)
		if err != nil {
			lastErr = err
			continue
		}
		if sourceIndex > 0 {
			log.Warn("上一个下载源不可用，改用镜像源", "source", base)
		}
		for attempt, version := range versions {
			if attempt >= nodeVersionAttempts {
				break
			}
			fileName := nodeFileName(version, platform)
			want, err := expectedSum(ctx, client, base, version, fileName)
			if err != nil {
				// 这个版本没有本平台的包（或列表取不到），往下试。
				lastErr = err
				continue
			}
			url := fmt.Sprintf("%s/%s/%s", base, version, fileName)
			log.Info("本机没有 npx，正在下载 Node.js 运行时（不改系统，只放进网关状态目录）",
				"version", version, "url", url, "dir", root)

			archive, err := os.CreateTemp(root, ".download-*")
			if err != nil {
				return "", fmt.Errorf("创建临时文件失败: %w", err)
			}
			archivePath := archive.Name()
			archive.Close()
			defer os.Remove(archivePath)

			if err := downloadVerified(ctx, client, url, want, archivePath); err != nil {
				log.Warn("下载 Node.js 失败，改试其它版本或源", "url", url, "err", errText(err))
				lastErr = err
				continue
			}

			binDir, err := unpackRuntime(archivePath, fileName, root)
			if err != nil {
				log.Warn("解包 Node.js 失败，改试其它版本或源", "file", fileName, "err", errText(err))
				lastErr = err
				continue
			}
			if err := smokeNodeRuntime(ctx, binDir); err != nil {
				// 缺系统库是**可以自动修**的一类：装上包再冒烟一次。最小安装的
				// Debian/Ubuntu 缺 libatomic1 是实测踩到的，用户不该为此读日志。
				//
				// 其它失败（glibc 太旧、架构不对）修不了，但**必须留下原因**：
				// 以前这里只删不吭声，用户看到的只是「找不到 npx」反复重试。
				if missing := missingSharedLibrary(err); missing != "" {
					log.Warn("自备的 Node.js 缺系统库，准备补装", "library", missing)
					if installErr := installMissingLibrary(ctx, log, missing); installErr == nil {
						if retryErr := smokeNodeRuntime(ctx, binDir); retryErr == nil {
							log.Info("补齐系统库后 Node.js 可用了", "version", version, "bin", binDir, "source", base)
							return binDir, nil
						} else {
							log.Warn("补装系统库后仍然跑不起来", "err", errText(retryErr))
						}
					} else {
						log.Warn("补装系统库没成功", "err", errText(installErr))
					}
					_ = os.RemoveAll(filepath.Dir(binDir))
					return "", &MissingLibraryError{Library: missing, Output: errText(err)}
				}
				log.Error("自备的 Node.js 运行时跑不起来", "version", version, "err", errText(err))
				_ = os.RemoveAll(filepath.Dir(binDir))
				return "", err
			}
			log.Info("Node.js 运行时已就绪", "version", version, "bin", binDir, "source", base)
			return binDir, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有找到可用的 Node.js 版本")
	}
	return "", fmt.Errorf("自动准备 Node.js 失败：%w；请手工安装 Node.js（https://nodejs.org/），或用 --dsh-cmd 指定启动命令", lastErr)
}

// unpackRuntime 把下载好的包解到状态目录里并返回 bin 目录。
//
// 先解到临时目录再改名：中途失败留下半个运行时的话，下一次启动会把它当成
// 「已装好」然后一直用坏的那份。
func unpackRuntime(archivePath, fileName, root string) (string, error) {
	tmpDir, err := os.MkdirTemp(root, ".extract-*")
	if err != nil {
		return "", fmt.Errorf("创建解包目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir) // 成功改名后这里是空操作

	if err := extractNodeArchive(archivePath, fileName, tmpDir); err != nil {
		return "", err
	}

	finalDir := filepath.Join(root, strings.TrimSuffix(strings.TrimSuffix(fileName, ".tar.gz"), ".zip"))
	// 多实例同时首次启动时会有两个进程各下一份。谁先落地算谁的：已经有一份
	// 能用的运行时就直接用，别把它删掉换成自己这份（那会踩掉正在用它的实例）。
	if runtimeUsable(nodeBinDir(finalDir)) {
		return nodeBinDir(finalDir), nil
	}
	if err := os.RemoveAll(finalDir); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("清理旧的运行时失败: %w", err)
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		// 改名失败可能是另一个进程刚刚抢先落地：用现成的那份。
		if runtimeUsable(nodeBinDir(finalDir)) {
			return nodeBinDir(finalDir), nil
		}
		return "", fmt.Errorf("就位运行时失败: %w", err)
	}

	binDir := nodeBinDir(finalDir)
	if !runtimeUsable(binDir) {
		return "", fmt.Errorf("解包出来的运行时不可用（%s）：包里可能缺 npx 或它依赖的文件",
			nodeNPXPath(binDir))
	}
	return binDir, nil
}

// LocalNodeBin 返回网关自备运行时的 bin 目录；没有装过时 ok=false。
//
// 给 `check` 用：判断「现在能不能拉起 dsh」时，不能只看系统 PATH。
func LocalNodeBin(stateDir string) (string, bool) {
	root := filepath.Join(stateDir, nodeDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		binDir := nodeBinDir(filepath.Join(root, entry.Name()))
		if runtimeUsable(binDir) {
			return binDir, true
		}
	}
	return "", false
}

// nodeBinDir 返回解包目录里放可执行文件的那一层。
//
// 官方 tar.gz 是 node-v22.11.0-linux-x64/bin/...，Windows 的 zip 则把
// node.exe 与 npx.cmd 直接放在根，所以按平台分开判断。
func nodeBinDir(extractDir string) string {
	if runtime.GOOS == "windows" {
		return extractDir
	}
	return filepath.Join(extractDir, "bin")
}

// nodeNPXPath 返回该 bin 目录里的 npx 路径。
func nodeNPXPath(binDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(binDir, "npx.cmd")
	}
	return filepath.Join(binDir, "npx")
}

// nodePlatformBlocker 返回当前平台不该走自动下载时的原因。
//
// Alpine（musl）上官方 glibc 二进制跑不起来，硬下下来只会在冒烟那一步失败，
// 不如直接说清楚该用什么。
func nodePlatformBlocker() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return "当前系统是 Alpine（musl），官方 Node.js 二进制需要 glibc 跑不起来；请用 apk add nodejs，或用 --dsh-cmd 指定启动命令"
	}
	return ""
}

// nodePlatformKey 把 GOOS/GOARCH 映射成 nodejs.org 用的平台名。
func nodePlatformKey() (string, error) {
	arch, ok := map[string]string{
		"amd64": "x64",
		"arm64": "arm64",
		"386":   "x86",
		"arm":   "armv7l",
	}[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("不认识的架构 %s，无法自动下载 Node.js；请手工安装 Node.js，或用 --dsh-cmd 指定启动命令", runtime.GOARCH)
	}
	switch runtime.GOOS {
	case "linux", "darwin":
		return runtime.GOOS + "-" + arch, nil
	case "windows":
		return "win-" + arch, nil
	default:
		return "", fmt.Errorf("不支持在 %s 上自动下载 Node.js；请手工安装，或用 --dsh-cmd 指定启动命令", runtime.GOOS)
	}
}

// nodeVersions 从某个源取版本列表，从新到旧。
func nodeVersions(ctx context.Context, client *http.Client, base string) ([]string, error) {
	raw, err := fetchSmall(ctx, client, base+"/index.json")
	if err != nil {
		return nil, err
	}
	versions, err := decodeNodeVersions(raw)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, errors.New("Node.js 版本列表是空的")
	}
	return versions, nil
}

// decodeNodeVersions 解析 index.json，按官方给的顺序（新到旧）返回版本号。
func decodeNodeVersions(raw []byte) ([]string, error) {
	var releases []nodeRelease
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, fmt.Errorf("解析 Node.js 版本列表失败: %w", err)
	}
	versions := make([]string, 0, len(releases))
	for _, release := range releases {
		if release.Version != "" {
			versions = append(versions, release.Version)
		}
	}
	return versions, nil
}

// expectedSum 从某个版本的 SHASUMS256.txt 里取目标包的校验和。
//
// 这个文件同时充当「该版本有没有发布这个平台的包」的判据：官方 index.json
// 的 files 字段用的是 osx-x64-tar 这类粗粒度名字，只有真实文件名才作数。
func expectedSum(ctx context.Context, client *http.Client, base, version, fileName string) (string, error) {
	sums, err := fetchSmall(ctx, client, fmt.Sprintf("%s/%s/SHASUMS256.txt", base, version))
	if err != nil {
		return "", err
	}
	want, ok := parseSHASUMS(string(sums), fileName)
	if !ok {
		return "", fmt.Errorf("%s 没有发布 %s", version, fileName)
	}
	return want, nil
}

// nodeFileName 拼出官方包名，如 node-v22.11.0-linux-x64.tar.gz。
func nodeFileName(version, platform string) string {
	ext := ".tar.gz"
	if strings.HasPrefix(platform, "win-") {
		ext = ".zip"
	}
	return "node-" + version + "-" + platform + ext
}

// downloadVerified 下载安装包并对着官方校验和验证，成功后写入 path。
//
// 边下边算哈希：40MB 的包不值得先在内存里放一份再算。
func downloadVerified(ctx context.Context, client *http.Client, url, want, path string) error {
	response, err := get(ctx, client, url)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("创建 %s 失败: %w", path, err)
	}

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hash), response.Body); err != nil {
		file.Close()
		return fmt.Errorf("下载 %s 失败: %w", url, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}

	got := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s 校验和不匹配（期望 %s，实际 %s）：下载内容不可信，已放弃", url, want, got)
	}
	return nil
}

// parseSHASUMS 从 SHASUMS256.txt 里取出某个文件的校验和。
func parseSHASUMS(text, fileName string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == fileName {
			return fields[0], true
		}
	}
	return "", false
}

// fetchSmall 取一个小文件（版本列表、校验和），限制大小以免被超大响应拖住。
func fetchSmall(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	response, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", url, err)
	}
	return raw, nil
}

// get 发一个带用户代理的 GET，非 200 直接报错。
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "dsh-gate")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("请求 %s 失败：HTTP %d", url, response.StatusCode)
	}
	return response, nil
}

// extractNodeArchive 按扩展名解包。
func extractNodeArchive(archivePath, fileName, dest string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("打开下载的包失败: %w", err)
	}
	defer file.Close()

	if strings.HasSuffix(fileName, ".zip") {
		info, err := file.Stat()
		if err != nil {
			return err
		}
		return extractZip(file, info.Size(), dest, 0)
	}
	return extractTarGz(file, dest, 1)
}

// extractTarGz 解开 tar.gz，strip 层前缀目录（官方包外面套了一层
// node-vX-平台/）。
//
// 符号链接必须原样重建：npm 与 npx 在包里就是指向 lib/node_modules 的软链，
// 跳过它们等于装了一个没有 npx 的 Node.js。
func extractTarGz(source io.Reader, dest string, strip int) error {
	gz, err := gzip.NewReader(source)
	if err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	defer gz.Close()

	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取压缩包失败: %w", err)
		}
		name := stripComponents(header.Name, strip)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(target, reader, header.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return fmt.Errorf("创建符号链接 %s 失败: %w", name, err)
			}
		case tar.TypeLink:
			source, err := safeJoin(dest, stripComponents(header.Linkname, strip))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return fmt.Errorf("创建硬链接 %s 失败: %w", name, err)
			}
		}
	}
	return nil
}

// extractZip 解开 zip（Windows 的官方包；解出来的文件直接放在根目录）。
func extractZip(reader io.ReaderAt, size int64, dest string, strip int) error {
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return fmt.Errorf("读取 zip 失败: %w", err)
	}
	for _, entry := range archive.File {
		name := stripComponents(entry.Name, strip)
		if name == "" {
			continue
		}
		target, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeFile(target, source, entry.Mode().Perm())
		source.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFile 写一个解包出来的文件，并保证权限生效。
//
// 官方包里 bin/ 下的可执行文件带 0755；这里对「没有任何执行位但位于 bin/ 下」
// 的情况再兜一层，权限位在 zip 里丢失时不至于解出一个不能执行的 node。
func writeFile(target string, source io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, source); err != nil {
		file.Close()
		return fmt.Errorf("写入 %s 失败: %w", target, err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	// OpenFile 的权限会被 umask 影响，显式再设一次。
	return os.Chmod(target, mode)
}

// stripComponents 去掉开头的 n 层目录，返回剩下的相对路径。
func stripComponents(name string, n int) string {
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[n:], "/")
}

// safeJoin 把包内路径拼到目标目录下，并挡住 `../` 这类逃逸。
//
// 压缩包内容来自网络，虽然已经校验过校验和，仍然不能让它写到目标目录之外。
func safeJoin(dest, name string) (string, error) {
	base := filepath.Clean(dest)
	target := filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return "", fmt.Errorf("压缩包里的路径 %q 指向了目标目录之外，已中止", name)
	}
	return target, nil
}

// smokeNodeRuntime 确认解出来的运行时真的能跑。
//
// 「解包成功」与「能执行」是两件事：glibc 版本不够时 tar 一切正常，跑起来才报错。
func smokeNodeRuntime(ctx context.Context, binDir string) error {
	ctx, cancel := context.WithTimeout(ctx, nodeSmokeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodeNPXPath(binDir), "--version")
	applyProbeAttrs(cmd)
	cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("自备的 Node.js 运行时无法执行（%v）：%s；请手工安装 Node.js 或用 --dsh-cmd 指定启动命令",
			err, strings.TrimSpace(string(output)))
	}
	return nil
}

// PrependPATH 把目录前置到 PATH 字符串里（导出给调用方拼环境变量用）。
func PrependPATH(dir, path string) string {
	if dir == "" {
		return path
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}

// MissingLibraryError 表示自备的 Node.js 缺一个系统共享库（例如 libatomic.so.1）。
//
// 单独成类型是因为它**可以自动修**：用一个包管理器装上对应的包即可，而其它失败
// （glibc 太旧、架构不对）只能给指引。
type MissingLibraryError struct {
	Library string
	Output  string
}

func (e *MissingLibraryError) Error() string {
	return fmt.Sprintf("自备的 Node.js 缺少系统库 %s：%s", e.Library, e.Output)
}

// missingSharedLibrary 从执行失败的输出里认出「缺哪个 .so」。
//
// 只认 loader 的原话（cannot open shared object file），不猜别的失败。
func missingSharedLibrary(err error) string {
	if err == nil {
		return ""
	}
	match := sharedLibraryPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return ""
	}
	return match[1]
}

// sharedLibraryPattern 匹配「libxxx.so.1: cannot open shared object file」。
var sharedLibraryPattern = regexp.MustCompile(`([\w.+-]+\.so[\w.+-]*)\s*:\s*cannot open shared object file`)

// runtimeUsable 判断一份自备运行时是否**真的能用**：npx 存在、且它的真实目标
// 也解出来了（官方包里 bin/npx 是指向 ../lib/node_modules/npm/bin/npx-cli.js 的
// 符号链接，只 os.Stat 链接本身会在目标缺失时误判为「装好了」）。
func runtimeUsable(binDir string) bool {
	npx := nodeNPXPath(binDir)
	info, err := os.Stat(npx)
	if err != nil || info.IsDir() {
		return false
	}
	// 符号链接：追到最后一跳，确认目标真的在。
	if resolved, err := filepath.EvalSymlinks(npx); err == nil {
		if _, err := os.Stat(resolved); err != nil {
			return false
		}
	}
	node := filepath.Join(binDir, "node")
	if runtime.GOOS == "windows" {
		node = filepath.Join(binDir, "node.exe")
	}
	if _, err := os.Stat(node); err != nil {
		return false
	}
	return true
}
