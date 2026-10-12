// Package dshsettings 定点读写 DSH 的设置与凭据文件。
//
// ## 为什么是「定点改写」而不是解析整个 YAML
//
// dsh-gate 坚持零第三方依赖，而 YAML 没有标准库实现。更要紧的是**写坏的代价**：
// `~/.dsh/settings.yaml` 是 DSH 的全部设置（服务商、模型、界面偏好……），
// 一份被我们改坏的 YAML 会让 DSH 起不来，而用户往往还不知道是哪一步坏的。
//
// 所以这里的策略是：
//
//   - 只做**行级**操作：按缩进找到某个键的块，整块替换或删除，其余行一个字节都不动；
//   - 写入前先备份成 `<文件>.dsh-gate.bak`；
//   - 写入后**回读校验**，对不上就还原备份并报错——宁可没配成功，也不能留一份坏的；
//   - 值只接受单行标量（不允许换行、制表符、行首特殊字符），从源头避免写出非法 YAML。
//
// 只覆盖本项目真正需要的形状（几个命名空间下的标量与一层列表），而不是通用 YAML：
// 遇到读不懂的结构就报错退出，绝不猜。
package dshsettings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SettingsFile 是设置文件名（在 dsh 家目录下）。
const SettingsFile = "settings.yaml"

// CredentialsFile 是凭据文件名（同一目录，含密钥，权限必须收紧）。
const CredentialsFile = ".credentials.yaml"

// 命名空间与键名：这些名字取自 DSH 自己插件的文档与注册代码
// （`@deepseek-ai/dsh-agent-default-model`、`@deepseek-ai/dsh-llm-pi-ai`），
// 不是猜的；provider 路由名 `deepseek-official` 见 dsh-llm-deepseek-api-key。
const (
	// NamespaceDefaultModel 是「新建会话默认用哪个服务商与模型」。
	NamespaceDefaultModel = "agent-default-model"
	// NamespacePiAI 是「自定义/第三方服务商」所在的命名空间。
	NamespacePiAI = "llm-pi-ai"
	// KeyProvider / KeyModel 是默认模型的两个必填项。
	KeyProvider = "provider"
	KeyModel    = "model"
)

// Paths 给出 dsh 家目录下设置与凭据文件的路径。
func Paths(dshHome string) (settings, credentials string) {
	return filepath.Join(dshHome, SettingsFile), filepath.Join(dshHome, CredentialsFile)
}

// Model 是一条模型条目。
type Model struct {
	ID   string
	Name string
}

// Provider 是一个自定义服务商。
type Provider struct {
	ID          string
	DisplayName string
	API         string // 例如 openai-completions
	BaseURL     string
	KeyEnv      string // 密钥所在的环境变量名，对应凭据文件 refs 里的键
	Models      []Model
}

// DeepSeekOfficial 是 DSH 自带的官方 DeepSeek 路由（无需在设置里定义）。
const DeepSeekOfficial = "deepseek-official"

// DeepSeekKeyEnv 是官方 DeepSeek 密钥默认对应的环境变量名。
const DeepSeekKeyEnv = "DEEPSEEK_API_KEY"

// DefaultModelRef 是「默认模型」的一条读写结果。
type DefaultModelRef struct {
	Provider string
	Model    string
	Set      bool
}

// doc 是一份待改写的文档：原始行 + 改动痕迹。
type doc struct {
	path    string
	lines   []string
	backup  string
	created bool
}

// open 读取文档；文件不存在时返回空文档（首次配置时 DSH 可能还没写过）。
func open(path string) (*doc, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &doc{path: path, created: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return &doc{path: path, lines: strings.Split(text, "\n")}, nil
}

// save 先备份，再原子替换；随后调用方负责回读校验。
func (d *doc) save(mode os.FileMode) error {
	if !d.created {
		if err := d.backupFile(); err != nil {
			return err
		}
	}
	// 行数组的末尾语义要对齐：open() 读进来的文件末尾换行会多出一个空元素，
	// 而新建文件时 d.lines 是 nil、追加的块没有那个空元素。不补的话，新写的
	// 文件末尾缺换行——合法但别扭（手工编辑时容易和下一行粘在一起）。
	lines := d.lines
	if len(lines) == 0 || lines[len(lines)-1] != "" {
		lines = append(append([]string{}, lines...), "")
	}
	body := strings.Join(lines, "\n")
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".dsh-gate-write-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("设置权限失败: %w", err)
	}
	if err := os.Rename(tmpPath, d.path); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", d.path, err)
	}
	return nil
}

// backupFile 留一份同目录备份，供回滚与人工排查。
func (d *doc) backupFile() error {
	raw, err := os.ReadFile(d.path)
	if err != nil {
		return fmt.Errorf("备份 %s 失败: %w", d.path, err)
	}
	d.backup = d.path + ".dsh-gate.bak"
	if err := os.WriteFile(d.backup, raw, 0o600); err != nil {
		return fmt.Errorf("写备份 %s 失败: %w", d.backup, err)
	}
	return nil
}

// rollback 用备份还原（回读校验失败时调用）。
func (d *doc) rollback() {
	if d.created {
		// 本来就没有这个文件（第一次配置时），回滚就是把它删掉。
		_ = os.Remove(d.path)
		return
	}
	if d.backup == "" {
		return
	}
	_ = os.WriteFile(d.path, mustRead(d.backup), 0o600)
}

// mustRead 读一个刚写下的备份文件，失败时返回空（回滚是尽力而为）。
func mustRead(path string) []byte {
	raw, _ := os.ReadFile(path)
	return raw
}

// indentOf 返回一行的缩进宽度（制表符按 1 算，因为 YAML 本身不允许用制表符缩进）。
func indentOf(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// isBlank 判空行与整行注释。
func isBlank(line string) bool {
	t := strings.TrimSpace(line)
	return t == "" || strings.HasPrefix(t, "#")
}

// keyOf 取「缩进 + 键名」。不是键行时返回 ok=false。
//
// 只认没有内联值的写法（`key:`），因为本包只改写块状结构：
// `foo: bar` 这种标量行属于别人的内容，不去碰。
func keyOf(line string) (indent int, key string, ok bool) {
	if isBlank(line) {
		return 0, "", false
	}
	indent = indentOf(line)
	trimmed := strings.TrimSpace(line)
	colon := strings.IndexByte(trimmed, ':')
	if colon <= 0 {
		return 0, "", false
	}
	key = strings.TrimSpace(trimmed[:colon])
	if key == "" || strings.ContainsAny(key, " \t\"'{}[],&*#?|<>=!%@`") {
		return 0, "", false
	}
	if strings.TrimSpace(trimmed[colon+1:]) != "" {
		return 0, "", false
	}
	return indent, key, true
}

// blockRange 返回某个键的块覆盖的行区间（含键行本身）。
func (d *doc) blockRange(parentIndent int, from, to int, key string) (int, int, bool) {
	for i := from; i < to; i++ {
		indent, k, ok := keyOf(d.lines[i])
		if !ok || indent <= parentIndent || k != key {
			continue
		}
		end := i + 1
		for j := i + 1; j < to; j++ {
			if isBlank(d.lines[j]) {
				continue
			}
			if indentOf(d.lines[j]) <= indent {
				break
			}
			end = j + 1
		}
		return i, end, true
	}
	return 0, 0, false
}

// lookup 按路径逐层找到块，例如 ["llm-pi-ai","providers","my-id"]。
func (d *doc) lookup(path []string) (int, int, bool) {
	if len(path) == 0 {
		return 0, len(d.lines), true
	}
	indent := -1
	from := 0
	to := len(d.lines)
	for i := 0; i < len(path); i++ {
		start, end, ok := d.blockRange(indent, from, to, path[i])
		if !ok {
			return 0, 0, false
		}
		indent, from, to = indentOf(d.lines[start]), start+1, end
	}
	// 返回块**内容**的范围（不含键行），并给出该块的缩进。
	if from > to {
		from = to
	}
	return from, to, true
}

// upsert 用 body 替换 path 对应的块内容；块不存在时，在**最深的已存在父块里**
// 补上缺失的那几层再写入。
//
// 这里曾经写错过一次，值得写下来：早先的实现找不到整条路径时，就把整条路径
// （连同 `llm-pi-ai:` 这个顶层键）追加到文件末尾，结果 YAML 里出现两个同名
// 顶层键——后者覆盖前者，DSH 原有的服务商被无声抹掉。YAML 允许重复键这件事
// 本身就是坑，宁可在这里多写几行把结构补对。
func (d *doc) upsert(path []string, body []string) error {
	if err := validScalarLines(body); err != nil {
		return err
	}

	// 找最深的已存在前缀。
	depth := 0
	for i := 1; i <= len(path); i++ {
		if _, _, ok := d.lookup(path[:i]); ok {
			depth = i
		} else {
			break
		}
	}

	switch depth {
	case len(path):
		// 整条路径都在：原地替换它的内容（键行也一起重写，缩进随之对齐）。
		start, end, _ := d.lookup(path)
		head := start - 1
		merged := append([]string{}, d.lines[:head]...)
		merged = append(merged, d.render(path, body)...)
		merged = append(merged, trimLeadingBlank(d.lines[end:])...)
		d.lines = merged
		return nil

	case 0:
		// 顶层都没有：整段追加到文件末尾。
		d.appendBlock(path, body)
		return nil

	default:
		// 父块在、补几层：在父块内容末尾插入缺失的键，再接 body。
		_, containerEnd, ok := d.lookup(path[:depth])
		if !ok {
			return fmt.Errorf("内部不一致：%v 的前 %d 层刚才还在", path, depth)
		}
		insert := d.renderAt(depth, path[depth:], body)
		// 父块内部最后一层的缩进决定新键该缩进多少：2 空格一层。
		tail := append([]string{}, d.lines[:containerEnd]...)
		tail = append(tail, insert...)
		tail = append(tail, d.lines[containerEnd:]...)
		d.lines = tail
		return nil
	}
}

// trimLeadingBlank 去掉切出来的尾段开头的空行（否则块与下一个键之间会多出空档）。
func trimLeadingBlank(lines []string) []string {
	for len(lines) > 0 && isBlank(lines[0]) {
		lines = lines[1:]
	}
	return lines
}

// appendBlock 把一个新块追加到文件末尾（键路径逐层补出来）。
func (d *doc) appendBlock(path []string, body []string) {
	for len(d.lines) > 0 && isBlank(d.lines[len(d.lines)-1]) {
		d.lines = d.lines[:len(d.lines)-1]
	}
	if len(d.lines) > 0 {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, d.render(path, body)...)
}

// render 把「键路径 + 内容行」渲染成缩进正确的块（从顶层开始）。
func (d *doc) render(path []string, body []string) []string {
	return d.renderAt(0, path, body)
}

// renderAt 同 render，但整体缩进 base 层（插入到已有块内部时用）。
func (d *doc) renderAt(base int, path []string, body []string) []string {
	var out []string
	for i, key := range path {
		indent := strings.Repeat("  ", base+i)
		out = append(out, indent+key+":")
	}
	indent := strings.Repeat("  ", base+len(path))
	for _, line := range body {
		if line == "" {
			out = append(out, "")
			continue
		}
		// body 里已带相对缩进的行直接平移；无缩进的行视为本层直属。
		out = append(out, indent+line)
	}
	return out
}

// setScalar 改写或补上一个**顶层标量**（`key: value` 那一行）。
//
// 凭据文件顶上那行 `version: 1` 就是这种东西：它不是块，本包的 upsert 用不上。
func (d *doc) setScalar(key, value string) {
	line := key + ": " + value
	for i, text := range d.lines {
		if isBlank(text) {
			continue
		}
		if indentOf(text) != 0 {
			continue
		}
		if name, _, ok := splitScalar(text); ok && name == key {
			d.lines[i] = line
			return
		}
	}
	// 文件里没有这一行：放到最前面（顶层键都该在文件开头，先加后加都不影响解析）。
	d.lines = append([]string{line}, d.lines...)
}

// splitScalar 拆一行顶层标量（`key: value`）。
func splitScalar(text string) (key, value string, ok bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	colon := strings.IndexByte(trimmed, ':')
	if colon <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(trimmed[:colon]), strings.TrimSpace(trimmed[colon+1:]), true
}

// remove 删掉路径对应的块（连键行一起）。
func (d *doc) remove(path []string) bool {
	// start/end 是块**内容**的范围，键行在 start-1；两者都要删。
	//
	// 这里曾经只从 start 往后跳过空行，结果把子键全留下了——删掉
	// `my-gw:` 之后它的字段变成孤儿，YAML 里就成了 `providers:` 的直接子项。
	start, end, ok := d.lookup(path)
	if !ok {
		return false
	}
	head := start - 1
	// 再吃掉块后面的空行，避免留下两处空档。
	for end < len(d.lines) && isBlank(d.lines[end]) {
		end++
	}
	tail := d.lines[end:]
	kept := append([]string{}, d.lines[:head]...)
	kept = append(kept, tail...)
	d.lines = kept
	return true
}

// scalars 读出块内容里属于本层直属的 `key: value` 标量。
func (d *doc) scalars(path []string) map[string]string {
	out := map[string]string{}
	start, end, ok := d.lookup(path)
	if !ok {
		return out
	}
	base := -1
	for i := start; i < end; i++ {
		if isBlank(d.lines[i]) {
			continue
		}
		indent := indentOf(d.lines[i])
		if base < 0 {
			base = indent
		}
		if indent != base {
			continue
		}
		trimmed := strings.TrimSpace(d.lines[i])
		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		value := strings.TrimSpace(trimmed[colon+1:])
		if key != "" && value != "" {
			out[key] = unquote(value)
		}
	}
	return out
}

// unquote 去掉标量值的引号。
func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// validScalarLines 拒绝会写出非法 YAML 的内容。
func validScalarLines(lines []string) error {
	// 容器键（`models:`）后面必须真的有更深缩进的内容，否则是残缺的块。
	containers := map[int]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.ContainsRune(line, '\t') {
			return fmt.Errorf("内容里不能有制表符: %q", line)
		}
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			continue // 列表项由调用方保证格式
		}
		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 {
			return fmt.Errorf("内容不是「键: 值」也不是列表项: %q", line)
		}
		value := strings.TrimSpace(trimmed[colon+1:])
		if strings.TrimSpace(value) == "" {
			containers[i] = true
			continue
		}
		if value[0] != '"' && value[0] != '\'' && !plainSafe(value) {
			return fmt.Errorf("值里有 YAML 特殊字符，请加引号: %q", value)
		}
	}
	for i := range containers {
		base := indentOf(lines[i])
		hasChild := false
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if indentOf(lines[j]) > base {
				hasChild = true
				break
			}
			break
		}
		if !hasChild {
			return fmt.Errorf("键 %q 后面没有内容", strings.TrimSpace(lines[i]))
		}
	}
	return nil
}

// plainSafe 判断不加引号是否安全（保守：只放行明显的普通文本）。
func plainSafe(value string) bool {
	switch value[0] {
	case '{', '[', '&', '*', '#', '|', '>', '%', '@', '`', '!', '?', '-', ',':
		return false
	}
	return !strings.Contains(value, ": ")
}

// yamlString 把值渲染成带引号的 YAML 标量。
func yamlString(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// Verify 回读校验：路径下的标量必须与期望一致，对不上就回滚。
func (d *doc) verify(path []string, want map[string]string) error {
	if err := d.save(0o600); err != nil {
		return err
	}
	reopened, err := open(d.path)
	if err != nil {
		d.rollback()
		return fmt.Errorf("回读 %s 失败: %w", d.path, err)
	}
	got := reopened.scalars(path)
	for key, value := range want {
		if got[key] != value {
			d.rollback()
			return fmt.Errorf("写入 %s 后校验不一致（%s = %q，期望 %q）；已还原备份 %s",
				d.path, key, got[key], value, d.backup)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 对外的四个动作
// ---------------------------------------------------------------------------

// SetDefaultModel 写入「新建会话默认用哪个服务商与模型」。
func SetDefaultModel(dshHome, provider, model string) (string, error) {
	if err := ensureHome(dshHome); err != nil {
		return "", err
	}

	if err := checkName(provider, "服务商名"); err != nil {
		return "", err
	}
	if err := checkName(model, "模型名"); err != nil {
		return "", err
	}
	path, _ := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return "", err
	}
	body := []string{
		KeyProvider + ": " + yamlString(provider),
		KeyModel + ": " + yamlString(model),
	}
	if err := document.upsert([]string{NamespaceDefaultModel}, body); err != nil {
		return "", fmt.Errorf("写入默认模型失败: %w", err)
	}
	if err := document.verify([]string{NamespaceDefaultModel}, map[string]string{
		KeyProvider: provider,
		KeyModel:    model,
	}); err != nil {
		return "", err
	}
	return path, nil
}

// ReadDefaultModel 读回默认模型。
func ReadDefaultModel(dshHome string) (DefaultModelRef, error) {
	path, _ := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return DefaultModelRef{}, err
	}
	values := document.scalars([]string{NamespaceDefaultModel})
	ref := DefaultModelRef{Provider: values[KeyProvider], Model: values[KeyModel]}
	ref.Set = ref.Provider != "" && ref.Model != ""
	return ref, nil
}

// SetKey 在凭据文件里写入一个环境变量名对应的密钥。
//
// DSH 的凭据文件是 `refs:` 下「环境变量名: 密钥」，插件用 apiKeyEnv 引用它
// （官方 DeepSeek 默认读 DEEPSEEK_API_KEY）。
func SetKey(dshHome, envName, secret string) (string, error) {
	if err := ensureHome(dshHome); err != nil {
		return "", err
	}

	if err := checkName(envName, "环境变量名"); err != nil {
		return "", err
	}
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("密钥不能为空")
	}
	if strings.ContainsAny(secret, "\r\n\t") {
		return "", fmt.Errorf("密钥里有非法字符")
	}
	_, path := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return "", err
	}
	// 凭据文件顶上必须有 `version: 1`：实测缺了它 DSH 直接拒绝启动
	// （credentials-local 把它当成「发布前的扁平布局」并要求迁移）。
	document.setScalar("version", CredentialsVersion)
	// path 指向容器（refs），body 是它的子项 —— 否则会写出重复的同名键。
	body := []string{envName + ": " + yamlString(secret)}
	if err := document.upsert([]string{"refs"}, body); err != nil {
		return "", fmt.Errorf("写入密钥失败: %w", err)
	}
	if err := document.verify([]string{"refs"}, map[string]string{envName: secret}); err != nil {
		return "", err
	}
	return path, nil
}

// CredentialsVersion 是凭据文件必须带的版本标记。
//
// 依据：实测 DSH 的 credentials-local 在缺这一行时拒绝启动，并提示
// 「Add `version: 1` and nest the existing entries under `refs:`」。
const CredentialsVersion = "1"

// ListKeys 列出凭据文件里已有的密钥名（不显示密钥本身）。
func ListKeys(dshHome string) ([]string, error) {
	_, path := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return nil, err
	}
	var names []string
	for name := range document.scalars([]string{"refs"}) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// HasKey 判断某个环境变量名是否已经在凭据文件里。
func HasKey(dshHome, envName string) (bool, error) {
	_, path := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return false, err
	}
	values := document.scalars([]string{"refs"})
	_, ok := values[envName]
	return ok, nil
}

// SetProvider 在设置文件里新增或覆盖一个自定义服务商。
func SetProvider(dshHome string, provider Provider) (string, error) {
	if err := ensureHome(dshHome); err != nil {
		return "", err
	}

	if err := checkName(provider.ID, "服务商 id"); err != nil {
		return "", err
	}
	body := []string{}
	if provider.DisplayName != "" {
		body = append(body, "displayName: "+yamlString(provider.DisplayName))
	}
	if provider.API != "" {
		body = append(body, "api: "+yamlString(provider.API))
	}
	if provider.BaseURL != "" {
		body = append(body, "baseURL: "+yamlString(provider.BaseURL))
	}
	if provider.KeyEnv != "" {
		body = append(body, "apiKeyEnv: "+yamlString(provider.KeyEnv))
	}
	if len(provider.Models) > 0 {
		body = append(body, "models:")
	}
	for _, model := range provider.Models {
		name := model.Name
		if name == "" {
			name = model.ID
		}
		body = append(body, "  - id: "+yamlString(model.ID))
		if name != model.ID {
			body = append(body, "    name: "+yamlString(name))
		}
	}
	path, _ := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return "", err
	}
	target := []string{NamespacePiAI, "providers", provider.ID}
	if err := document.upsert(target, body); err != nil {
		return "", fmt.Errorf("写入服务商失败: %w", err)
	}
	if err := document.verify(target, map[string]string{"baseURL": provider.BaseURL}); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveProvider 删除一个自定义服务商。
func RemoveProvider(dshHome, id string) (string, bool, error) {
	if err := ensureHome(dshHome); err != nil {
		return "", false, err
	}

	if err := checkName(id, "服务商 id"); err != nil {
		return "", false, err
	}
	path, _ := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return "", false, err
	}
	target := []string{NamespacePiAI, "providers", id}
	if _, _, ok := document.lookup(target); !ok {
		return path, false, nil
	}
	if !document.remove(target) {
		return path, false, nil
	}
	if err := document.save(0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// ensureHome 确保 dsh 实例目录存在。
//
// 为什么写文件之前一定要做：`dsh-gate model key set --dsh-home <新目录>` 与首次
// setup 都可能面对一个还不存在的目录，而写盘走的是「同目录下建临时文件再改名」，
// 目录不在就会得到一句 `open …/.dsh-gate-write-…: no such file or directory`。
// 用户看到的是「配模型失败」，实际只是目录没建。
//
// 权限取 0700：里面有凭据。
func ensureHome(dshHome string) error {
	if strings.TrimSpace(dshHome) == "" {
		return errors.New("dsh 实例目录为空")
	}
	if err := os.MkdirAll(dshHome, 0o700); err != nil {
		return fmt.Errorf("创建 dsh 实例目录 %s 失败: %w", dshHome, err)
	}
	return nil
}

// RemoveKey 删掉一个密钥名。
//
// 只删 refs 下的那一项；文件里没有它时返回 found=false（不算错）。
func RemoveKey(dshHome, envName string) (string, bool, error) {
	if err := ensureHome(dshHome); err != nil {
		return "", false, err
	}

	if err := checkName(envName, "环境变量名"); err != nil {
		return "", false, err
	}
	_, path := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return "", false, err
	}
	target := []string{"refs", envName}
	if _, _, ok := document.lookup(target); !ok {
		return path, false, nil
	}
	// 这里必须走「删掉整个键行」的路径：refs 下每个键都是一行标量，
	// 而本包的 remove 只认块。先取出当前值，再重写 refs 块（跳过这个键）。
	if _, _, ok := document.blockRange(0, 0, len(document.lines), "refs"); !ok {
		return path, false, nil
	}
	refsStart, refsEnd, _ := document.lookup([]string{"refs"})
	var kept []string
	for i := refsStart; i < refsEnd; i++ {
		key := strings.TrimSpace(document.lines[i])
		if strings.HasPrefix(key, envName+":") {
			continue
		}
		kept = append(kept, document.lines[i])
	}
	head := refsStart - 1
	merged := append([]string{}, document.lines[:head+1]...)
	merged = append(merged, kept...)
	merged = append(merged, document.lines[refsEnd:]...)
	document.lines = merged
	if err := document.save(0o600); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// ListProviders 列出设置文件里自定义的服务商。
type ProviderInfo struct {
	ID          string
	DisplayName string
	API         string
	BaseURL     string
	KeyEnv      string
	Models      []string
}

// ListProviders 读出 llm-pi-ai 下自定义的服务商。
func ListProviders(dshHome string) ([]ProviderInfo, error) {
	path, _ := Paths(dshHome)
	document, err := open(path)
	if err != nil {
		return nil, err
	}
	start, end, ok := document.lookup([]string{NamespacePiAI, "providers"})
	if !ok {
		return nil, nil
	}
	base := -1
	var out []ProviderInfo
	for i := start; i < end; i++ {
		if isBlank(document.lines[i]) {
			continue
		}
		indent := indentOf(document.lines[i])
		if base < 0 {
			base = indent
		}
		if indent != base {
			continue
		}
		_, id, isKey := keyOf(document.lines[i])
		if !isKey {
			continue
		}
		info := ProviderInfo{ID: id}
		values := document.scalars([]string{NamespacePiAI, "providers", id})
		info.DisplayName = values["displayName"]
		info.API = values["api"]
		info.BaseURL = values["baseURL"]
		info.KeyEnv = values["apiKeyEnv"]
		info.Models = modelsIn(document, []string{NamespacePiAI, "providers", id, "models"})
		out = append(out, info)
	}
	return out, nil
}

// modelsIn 读出某个块下 `models:` 列表里的 id。
func modelsIn(document *doc, path []string) []string {
	start, end, ok := document.lookup(path)
	if !ok {
		return nil
	}
	var out []string
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(document.lines[i])
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		rest := strings.TrimSpace(trimmed[2:])
		if !strings.HasPrefix(rest, "id:") {
			continue
		}
		out = append(out, unquote(strings.TrimSpace(strings.TrimPrefix(rest, "id:"))))
	}
	return out
}

// checkName 拒绝会写出非法 YAML 的名字。
func checkName(name, label string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("%s不能为空", label)
	}
	if strings.ContainsAny(trimmed, " \t\r\n:\"'#{}[],") {
		return fmt.Errorf("%s %q 里含有非法字符（不能有空格、冒号、引号或 #）", label, name)
	}
	return nil
}

// BackupPath 返回某个文件对应的备份路径（供文档与提示使用）。
func BackupPath(path string) string { return path + ".dsh-gate.bak" }

// Stamp 返回一个可读的时间戳（用于文件名）。
func Stamp() string { return time.Now().Format("20060102-150405") }
