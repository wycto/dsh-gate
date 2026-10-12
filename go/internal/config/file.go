package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// File 是多实例配置文件的结构。
//
// 为什么用 JSON 而不是 YAML/TOML：本项目刻意保持零第三方依赖，标准库只
// 认 JSON。与其为了好看的语法引入一棵依赖树，不如让配置文件朴素一点——
// 它只需要写一次，通常还是从 README 的示例里复制改的。
//
// 语法错误会**立即报错**（DisallowUnknownFields）：把 trustedHosts 拼成
// trustedHost 时，静默忽略会让人以为配置生效了，实际却在裸奔。
type File struct {
	// Defaults 是所有实例共享的公共配置。每个实例从它出发再叠加自己的字段。
	Defaults Instance `json:"defaults,omitempty"`
	// Instances 是要启动的实例列表。
	Instances []Instance `json:"instances"`
}

// LoadFile 读取多实例配置文件，逐个补齐默认值并校验。
func LoadFile(path string) ([]Instance, error) {
	abs := ExpandPath(path)
	if abs == "" {
		return nil, errors.New("配置文件路径为空")
	}
	if !filepath.IsAbs(abs) {
		absolute, err := filepath.Abs(abs)
		if err != nil {
			return nil, fmt.Errorf("解析配置文件路径失败 %q: %w", path, err)
		}
		abs = absolute
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败 %q: %w", abs, err)
	}

	// 一次性把整份文档严格解出来：任何拼错的键都在这里报错，而不是被静默忽略。
	// （曾经用两次解码分别取 defaults 与 instances，结果第二次的严格解码会把
	// 第一次已经放过��的 instances 判成「未知字段」。）
	var document struct {
		Defaults  json.RawMessage   `json:"defaults,omitempty"`
		Instances []json.RawMessage `json:"instances"`
	}
	if err := decodeStrict(raw, &document); err != nil {
		return nil, fmt.Errorf("解析配置文件 %q 失败: %w", abs, err)
	}
	if len(document.Instances) == 0 {
		return nil, fmt.Errorf("配置文件 %q 的 instances 为空，至少要有一个实例", abs)
	}

	// defaults 解到一个填好默认值的 Instance 上，再让每个实例从它复制——
	// 这样写配置文件的人只需要写差异化字段。
	base := Default()
	if len(document.Defaults) > 0 && string(document.Defaults) != "null" {
		if err := decodeStrict(document.Defaults, &base); err != nil {
			return nil, fmt.Errorf("解析 %q 的 defaults 段失败: %w", abs, err)
		}
	}

	instances := make([]Instance, 0, len(document.Instances))
	seenName := map[string]bool{}
	for index, item := range document.Instances {
		instance := base
		if err := decodeStrict(item, &instance); err != nil {
			return nil, fmt.Errorf("解析 %q 第 %d 个实例失败: %w", abs, index+1, err)
		}
		if err := instance.Normalize(); err != nil {
			return nil, fmt.Errorf("配置文件 %q 第 %d 个实例（%s）配置有误: %w", abs, index+1, instance.Name, err)
		}
		if seenName[instance.Name] {
			return nil, fmt.Errorf("配置文件 %q 里实例名 %q 重复了", abs, instance.Name)
		}
		seenName[instance.Name] = true
		// 网关端口互相冲突：同一进程里没法绑两次。
		for _, previous := range instances {
			if addrOverlap(previous.Host, previous.Port, instance.Host, instance.Port) {
				return nil, fmt.Errorf("实例 %q 与 %q 的网关端口都是 %d，同一进程里无法共存",
					instance.Name, previous.Name, instance.Port)
			}
		}
		for _, previous := range instances {
			// dsh 端口互相冲突：两个网关抢同一个 dsh，转发过去的结果无法解释。
			if addrOverlap(previous.DSHHost, previous.DSHPort, instance.DSHHost, instance.DSHPort) {
				return nil, fmt.Errorf("实例 %q 与 %q 的 dsh 端口都是 %d，它们会抢同一个 dsh",
					instance.Name, previous.Name, instance.DSHPort)
			}
			// 跨角色冲突：一个实例的网关端口撞上另一个实例的 dsh 端口。
			if addrOverlap(previous.Host, previous.Port, instance.DSHHost, instance.DSHPort) {
				return nil, fmt.Errorf("实例 %q 的网关端口 %d 撞上了实例 %q 的 dsh 端口（%s）",
					instance.Name, instance.Port, previous.Name, previous.DSHAddr())
			}
			if addrOverlap(previous.DSHHost, previous.DSHPort, instance.Host, instance.Port) {
				return nil, fmt.Errorf("实例 %q 的网关端口 %d 撞上了实例 %q 的 dsh 端口（%s）",
					instance.Name, instance.Port, previous.Name, previous.DSHAddr())
			}
		}
		instances = append(instances, instance)
	}
	return instances, nil
}

// decodeStrict 用标准库解码 JSON，并拒绝未知字段。
func decodeStrict(raw []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	// 拒绝尾部垃圾：{"instances":[]} oops 这种手滑应当报错而不是被忽略。
	if decoder.More() {
		return errors.New("JSON 后面还有多余内容")
	}
	return nil
}

// addrOverlap 判断两个监听地址是否可能抢占同一个端口。
//
// 只有端口相同**且**主机地址有交集时才算冲突：一个绑 127.0.0.1 的网关和一个
// 绑 192.168.1.5 的网关用同一个端口是允许的，而任何一个绑通配地址（0.0.0.0 /
// ::）就会和所有同端口的监听者打架。
func addrOverlap(hostA string, portA int, hostB string, portB int) bool {
	if portA != portB {
		return false
	}
	return hostOverlap(hostA, hostB)
}

// hostOverlap 判断两个监听主机是否有交集。
func hostOverlap(a, b string) bool {
	if a == b {
		return true
	}
	// 通配地址与任何地址都重叠。
	if isWildcardHost(a) || isWildcardHost(b) {
		return true
	}
	return false
}

// isWildcardHost 判断是否是不限网卡的监听地址。
func isWildcardHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}
