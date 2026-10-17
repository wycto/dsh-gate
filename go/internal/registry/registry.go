// Package registry 记录「本机上正在运行哪些网关实例」。
//
// 为什么需要它：一个网关实例知道自己监听哪个端口，但从外面看，端口号是配置
// 决定的、用 `--config` 还能一次起好几个——`status` 之类的命令没有别的办法
// 知道该去哪里看。登记一份就能回答三个问题：现在有几个实例在跑、分别叫什么
// 名字、它们的 dsh 与账号文件分别放在哪。
//
// 登记里还存了**当初的完整命令行**，这样 `restart` 才能用同样的参数把它再拉起
// 一次，而不必让人凭记忆复述一长串选项。
//
// 登记只是「尽力而为」的记录，不是锁：进程被强杀时来不及删除，条目就会残留。
// 因此读出来之后一律要用端口是否还在监听来复核，残留的条目会被标成「未运行」。
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Record 是一个正在运行的网关实例。
type Record struct {
	// Name 实例名，与 --name 一致。
	Name string `json:"name"`
	// PID 网关进程号。
	PID int `json:"pid"`
	// Exe 是网关可执行文件的绝对路径；restart 用它重新拉起自己。
	Exe string `json:"exe"`
	// Argv 是当初的完整命令行，第一个元素是程序名。
	Argv []string `json:"argv"`
	// Listen 网关监听地址。
	Listen string `json:"listen"`
	// Port 网关端口。
	Port int `json:"port"`
	// DSHPort dsh 端口。
	DSHPort int `json:"dshPort"`
	// DSHHome dsh 的实例目录。
	DSHHome string `json:"dshHome"`
	// UsersFile 账号表路径。
	UsersFile string `json:"usersFile"`
	// StateDir 网关状态目录。
	StateDir string `json:"stateDir"`
	// PublicURL 对外公告地址。
	PublicURL string `json:"publicUrl,omitempty"`
	// TLS 表示是否以 HTTPS 对外服务。
	TLS bool `json:"tls,omitempty"`
	// Managed 表示 dsh 是不是这个网关拉起来的（false 是接管的外部实例）。
	Managed bool `json:"managed"`
	// DSHManaged 由网关托管的 dsh 进程号，0 表示没有。
	DSHManagedPID int `json:"dshManagedPid,omitempty"`
	// Version 网关版本号。
	Version string `json:"version"`
	// StartedAt 启动时间。
	StartedAt time.Time `json:"startedAt"`
}

// Path 返回登记条目在磁盘上的位置。
//
// 必须走 Expand：目录本身可能带 `~`（默认就是 `~/.dsh-gate/run`）。此前这里直接用
// 未展开的 Dir，于是 Save 的 rename 目标与 Load/Remove 的读取路径都指向一个字面量
// `~` 目录；在 Windows 上更直接——`filepath.Join` 产出的是 `~\...`，expandPath 只认
// `~/`，于是 `dsh-gate run` 会把登记写进**当前目录下的 `~` 文件夹**，换个目录再
// `status` 就找不到自己刚起的实例。
func (d Dir) Path(name string) string {
	return filepath.Join(string(d.Expand()), sanitize(name)+".json")
}

// sanitize 把实例名变成安全的文件名。
//
// 实例名本身已经限制为字母数字与 - _，这里再挡一层是为了防止将来放宽校验之后
// 出现 "../../etc/passwd" 这类路径穿越——登记文件是自动写入的，不该成为漏洞。
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unnamed"
	}
	return b.String()
}

// Dir 是登记目录。
type Dir string

// DefaultDir 返回登记目录的默认位置。
//
// **刻意不跟随 --state-dir**：--state-dir 管的是单个实例的账号表，而「本机现在
// 跑着哪些网关」是机器级的事实。把它固定下来，status / stop / restart 才不必
// 先知道实例配置就能找上门。
func DefaultDir() Dir { return Dir(filepath.Join(DefaultStateDir, "run")) }

// DefaultStateDir 与 config.DefaultStateDir 保持一致；这里单独写死是为了不
// 让最底层的存储逻辑反向依赖配置包。
const DefaultStateDir = "~/.dsh-gate"

// Expand 展开目录里的 `~`。
func (d Dir) Expand() Dir { return Dir(expandPath(string(d))) }

// ensure 创建登记目录。
func (d Dir) ensure() error {
	path := d.Expand()
	return os.MkdirAll(string(path), 0o700)
}

// Save 写入一条登记。
//
// 先写临时文件再 rename：登记文件被读到一半会让 status 少列一个实例，而这种
// 「看起来只跑了一个」的误导比报错更难查。
func (d Dir) Save(record Record) error {
	path := d.Expand()
	if err := d.ensure(); err != nil {
		return fmt.Errorf("创建登记目录失败 %q: %w", path, err)
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化登记失败: %w", err)
	}
	raw = append(raw, '\n')

	target := d.Path(record.Name)
	tmp, err := os.CreateTemp(string(path), ".registry-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("写入登记失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("替换登记失败 %q: %w", target, err)
	}
	return nil
}

// Remove 删除一条登记。文件不存在不算错误。
func (d Dir) Remove(name string) error {
	err := os.Remove(d.Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("删除登记失败: %w", err)
	}
	return nil
}

// Load 读出一条登记。
func (d Dir) Load(name string) (Record, bool, error) {
	raw, err := os.ReadFile(d.Path(name))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("读取登记失败: %w", err)
	}
	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		return Record{}, false, fmt.Errorf("登记文件 %q 损坏: %w", d.Path(name), err)
	}
	return record, true, nil
}

// List 读出全部登记，按实例名排序。
//
// 单个文件损坏时不中断整体：其余实例的状态仍然有价值，把它标成读不出来继续
// 往下走，比整条命令失败更实用。
func (d Dir) List() ([]Record, []error) {
	entries, err := os.ReadDir(string(d.Expand()))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{fmt.Errorf("读取登记目录失败: %w", err)}
	}

	var (
		records  []Record
		problems []error
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		record, ok, err := d.Load(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if ok {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, problems
}

// Find 按名字取登记；名字为空时返回全部。
func (d Dir) Find(names []string) ([]Record, error) {
	all, problems := d.List()
	if len(problems) > 0 {
		return nil, problems[0]
	}
	if len(names) == 0 {
		return all, nil
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	var picked []Record
	for _, record := range all {
		if wanted[record.Name] {
			picked = append(picked, record)
			delete(wanted, record.Name)
		}
	}
	// 点名了却没找到，是用户的命令写错了，必须说出来——静默跳过会让人以为
	// 「重启了两个」其实只重启了一个。
	var missing []string
	for name := range wanted {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("没有找到这些实例的运行记录：%s", strings.Join(missing, "、"))
	}
	return picked, nil
}

// Alive 复核一条登记是不是真的还在跑。
//
// 判定用「端口是否还在监听」而不是「进程号是否存在」：进程号会被系统回收复用，
// 端口不会。进程号只作为辅助信息显示，不作为判据。
func (r Record) Alive() bool {
	if r.Port <= 0 {
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", r.Port))
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// expandPath 展开开头的 `~`。
//
// 两种分隔符都要认：`filepath.Join("~/.dsh-gate", "run")` 在 Windows 上产出的是
// `~\\.dsh-gate\\run`，只判 `~/` 会让默认目录永远展不开（config.ExpandPath 里
// 一直是两种都判的，这份私有副本漏了反斜杠那一支）。
func expandPath(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
