package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/dshsettings"
	"github.com/wycto/dsh-gate/internal/term"
)

// modelCommand 实现 `dsh-gate model`：在**装不了 dsh-dock** 时也能把模型配好。
//
// 为什么需要它：DSH 自带的模型设置页依赖 `@deepseek-ai/dsh-web-app` 挂载的
// `ctx.remote.settings`，而在只装 `dsh-gate` 的干净服务器上，那条路经常走不通
// （页面报 settings are unavailable in this browser）。`dsh-dock` 的模型设置页
// 是官方之外的另一条路，但它同样装不上时，用户手上就只剩这一条命令了。
//
// 写的是 DSH 自己的两个文件（`internal/dshsettings` 负责定点改写 + 备份 +
// 回读校验）：
//
//   - `<dsh-home>/.credentials.yaml` 的 `refs:`：按**环境变量名**存密钥
//     （官方 DeepSeek 默认读 `DEEPSEEK_API_KEY`，见 dsh-llm-deepseek-api-key）；
//   - `<dsh-home>/settings.yaml`：`agent-default-model`（默认服务商与模型）
//     与 `llm-pi-ai.providers`（自定义服务商）。
//
// 改完可能需要重启 dsh 才会生效；命令结束时会说明。
func modelCommand(args []string) int {
	if len(args) == 0 {
		printModelUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return modelListCommand(args[1:])
	case "key":
		return modelKeyCommand(args[1:])
	case "provider":
		return modelProviderCommand(args[1:])
	case "default":
		return modelDefaultCommand(args[1:])
	case "-h", "--help", "help":
		fs := flag.NewFlagSet("model", flag.ContinueOnError)
		var opts modelOptions
		registerModelFlags(fs, &opts)
		printModelUsage(os.Stdout)
		fs.PrintDefaults()
		return 0
	default:
		return usageError("未知的 model 子命令 %q，可用：list / key / provider / default", args[0])
	}
}

// printModelUsage 打印 model 用法。
func printModelUsage(w *os.File) {
	fmt.Fprint(w, `用法: dsh-gate model <list|key|provider|default> --instance <实例名|id> [参数] [选项]

在 DSH 装不了插件（模型设置页用不了）时，用它配置某个实例的服务商、密钥与默认模型。
写的是该实例自己 DSH_HOME 下的设置文件，改动前会自动备份，改完会回读校验。

实例用 --instance 指定（dsh-gate dsh list 查看）；模型配置是「每个实例各一份」的。

  dsh-gate model list --instance 演示实例
  dsh-gate model key set DEEPSEEK_API_KEY --stdin --instance 演示实例
  dsh-gate model key list --instance 演示实例
  dsh-gate model provider add my-gw --base-url https://…/v1 --model qwen3-max --instance 演示实例
  dsh-gate model provider remove my-gw --instance 演示实例
  dsh-gate model default deepseek-official deepseek-chat --instance 演示实例

官方 DeepSeek 是 DSH 自带的路由（deepseek-official），不需要 provider add，
只要配好密钥再把它设成默认即可。

选项:
`)
}

// modelOptions 是 model 各子命令共用的输入。
type modelOptions struct {
	Instance string
	StateDir string
	DB       string
	// DSHHome 是显式指定目录的兼容入口（脚本/测试用）；正常应当用 Instance。
	DSHHome string
}

// registerModelFlags 注册共用选项。
func registerModelFlags(fs *flag.FlagSet, opts *modelOptions) {
	fs.StringVar(&opts.Instance, "instance", "", "dsh 实例名或 id（必填；dsh-gate dsh list 查看）")
	fs.StringVar(&opts.Instance, "i", "", "同 --instance")
	fs.StringVar(&opts.StateDir, "state-dir", "", "网关状态目录（找 portal.db 用；默认 ~/.dsh-gate）")
	fs.StringVar(&opts.DB, "db", "", "管理端数据库路径（默认 <状态目录>/portal.db）")
	fs.StringVar(&opts.DSHHome, "dsh-home", "", "直接指定 dsh 实例目录（兼容/高级用法，优先于 --instance）")
}

// home 返回要操作的 dsh 家目录：显式 --dsh-home 优先，否则按 --instance 从管理端库解析。
func (o modelOptions) home() (string, error) {
	if strings.TrimSpace(o.DSHHome) != "" {
		return config.ExpandPath(o.DSHHome), nil
	}
	home, _, err := instanceHomeFromFlag(o.Instance, o.StateDir, o.DB)
	return home, err
}

// modelListCommand 打印当前的服务商、密钥名与默认模型。
func modelListCommand(args []string) int {
	fs := flag.NewFlagSet("model list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts modelOptions
	registerModelFlags(fs, &opts)
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	home, err := opts.home()
	if err != nil {
		return usageError("%v", err)
	}
	settingsPath, credentialsPath := dshsettings.Paths(home)

	fmt.Printf("dsh 实例目录  %s\n", home)
	fmt.Printf("  设置文件    %s\n", settingsPath)
	fmt.Printf("  凭据文件    %s\n", credentialsPath)

	ref, err := dshsettings.ReadDefaultModel(home)
	if err != nil {
		return fatal("读取默认模型失败: %v", err)
	}
	if ref.Set {
		fmt.Printf("默认模型      %s / %s\n", ref.Provider, ref.Model)
	} else {
		fmt.Println("默认模型      未设置")
	}

	keys, err := dshsettings.ListKeys(home)
	if err != nil {
		return fatal("读取密钥失败: %v", err)
	}
	if len(keys) == 0 {
		fmt.Println("已配密钥      无（模型可用之前必须先配一个）")
	} else {
		fmt.Printf("已配密钥      %s\n", strings.Join(keys, "、"))
	}

	providers, err := dshsettings.ListProviders(home)
	if err != nil {
		return fatal("读取服务商失败: %v", err)
	}
	fmt.Println()
	fmt.Println("官方路由")
	fmt.Printf("  %-16s 密钥 %s（配置里有这个密钥名即可用）\n",
		dshsettings.DeepSeekOfficial, dshsettings.DeepSeekKeyEnv)
	if len(providers) == 0 {
		fmt.Println()
		fmt.Println("自定义服务商（settings.yaml 的 llm-pi-ai.providers）  无")
		return 0
	}
	fmt.Println()
	fmt.Println("自定义服务商（settings.yaml 的 llm-pi-ai.providers）")
	for _, item := range providers {
		name := item.DisplayName
		if name == "" {
			name = "-"
		}
		fmt.Printf("  %-16s %s  %s\n", item.ID, item.BaseURL, name)
		if len(item.Models) > 0 {
			fmt.Printf("  %-16s 模型 %s\n", "", strings.Join(item.Models, "、"))
		}
		if item.KeyEnv != "" {
			fmt.Printf("  %-16s 密钥 %s\n", "", item.KeyEnv)
		}
	}
	return 0
}

// modelKeyCommand 管理密钥名（值本身永远不回显）。
func modelKeyCommand(args []string) int {
	if len(args) == 0 {
		return usageError("用法: dsh-gate model key <list|set|remove> [参数]")
	}
	sub := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("model key "+sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		opts      modelOptions
		fromStdin bool
	)
	registerModelFlags(fs, &opts)
	fs.BoolVar(&fromStdin, "stdin", false, "从标准输入读取密钥（推荐，避免出现在 shell 历史与进程列表里）")
	if err := parseFlexible(fs, rest); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	home, err := opts.home()
	if err != nil {
		return usageError("%v", err)
	}

	switch sub {
	case "list":
		names, err := dshsettings.ListKeys(home)
		if err != nil {
			return fatal("读取密钥失败: %v", err)
		}
		if len(names) == 0 {
			fmt.Println("没有配置任何密钥。")
			return 0
		}
		for _, name := range names {
			fmt.Println(name)
		}
		return 0

	case "set", "remove":
		names := fs.Args()
		if len(names) != 1 {
			return usageError("用法: dsh-gate model key %s <环境变量名> [--stdin]", sub)
		}
		envName := names[0]

		if sub == "remove" {
			path, removed, err := dshsettings.RemoveKey(home, envName)
			if err != nil {
				return fatal("删除密钥失败: %v", err)
			}
			if !removed {
				fmt.Printf("%s 里没有 %s，无需删除。\n", path, envName)
				return 0
			}
			fmt.Printf("已从 %s 删除密钥名 %s\n", path, envName)
			printModelReloadHint(home)
			return 0
		}

		secret, err := readSecret(fromStdin, envName)
		if err != nil {
			return usageError("%v", err)
		}
		path, err := dshsettings.SetKey(home, envName, secret)
		if err != nil {
			return fatal("写入密钥失败: %v", err)
		}
		fmt.Printf("已写入 %s（键名 %s，密钥本身不回显、不进日志）\n", path, envName)
		fmt.Println("提示：密钥以明文存在该文件里，请确认目录权限只对你的账号可读。")
		printModelReloadHint(home)
		return 0

	default:
		return usageError("未知的 model key 子命令 %q，可用：list / set / remove", sub)
	}
}

// readSecretMasked 在交互式终端里读一行，**逐字符回显成星号**。
//
// 不用「完全不回显」的读法：用户在终端里看不到任何反馈，会以为程序没在等输入
// （实测反馈就是「输入了但屏幕上什么都没有」）。星号既是反馈，也不泄露内容。
// 拿不到原始模式（非终端、或平台未适配）时退回完全不回显的写法。
func readSecretMasked(prompt string) (string, error) {
	if raw, err := term.ReadPasswordMasked(os.Stdin, os.Stdout, prompt); err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	raw, err := term.ReadPassword(os.Stdin, os.Stdout, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// readNewPassword 问两遍新口令，两遍都用星号回显；不一致就在原地重问。
//
// 为什么要两遍：口令是**看不见**的，敲错一个字符当场不会有人发现，要等到下次
// 登录才暴露——那时人已经离开终端了。第一次就输错还会把管理端锁在一个谁也进
// 不去的状态里，只能上机器重设。两遍比对是唯一能在当场发现手滑的办法。
//
// 为什么要星号：完全不回显时终端停在提示语后面一动不动，用户不知道程序到底
// 在不在等输入（实测反馈就是「输入了但屏幕上什么都没有」）。星号既给反馈，
// 也只泄露长度。
//
// 不一致只重问口令这两行，不把整个向导推倒重来（install 的向导一旦 continue
// 会把前面所有问题再问一遍，因为 visited 只记命令行给过的项）。
// Ctrl-C 由下面的读取层转成 ErrInterrupted 向上抛，这里不当成「再来一次」。
func readNewPassword(firstPrompt, secondPrompt string) (string, error) {
	for {
		first, err := readSecretMasked(firstPrompt)
		if err != nil {
			return "", err
		}
		second, err := readSecretMasked(secondPrompt)
		if err != nil {
			return "", err
		}
		if first != second {
			fmt.Println("  两次输入的口令不一致，请重试。")
			continue
		}
		return first, nil
	}
}

// readSecret 取密钥：优先标准输入，其次命令行（仅用于脚本化场景）。
func readSecret(fromStdin bool, envName string) (string, error) {
	if fromStdin {
		reader := bufio.NewReader(os.Stdin)
		value, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("从标准输入读取密钥失败: %w", err)
		}
		return strings.TrimSpace(value), nil
	}
	return "", fmt.Errorf("出于安全考虑不提供命令行明文参数，请用 --stdin 从标准输入读取：printf '%%s' \"$KEY\" | dsh-gate model key set %s --stdin", envName)
}

// modelProviderCommand 增删自定义服务商。
func modelProviderCommand(args []string) int {
	if len(args) == 0 {
		return usageError("用法: dsh-gate model provider <add|remove|list> [参数]")
	}
	sub := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet("model provider "+sub, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		opts       modelOptions
		baseURL    string
		api        string
		keyEnv     string
		showName   string
		modelsFlag string
	)
	registerModelFlags(fs, &opts)
	fs.StringVar(&baseURL, "base-url", "", "服务商地址，例如 https://api.example.com/v1")
	fs.StringVar(&api, "api", "openai-completions", "接口形态：openai-completions / anthropic-messages 等（按服务商要求填）")
	fs.StringVar(&keyEnv, "key-env", "", "密钥对应的环境变量名；先用 dsh-gate model key set <名字> 写入密钥")
	fs.StringVar(&showName, "name", "", "显示名（可选）")
	fs.StringVar(&modelsFlag, "models", "", "模型列表，逗号分隔；留空则不写模型（DSH 会自己发现）")
	if err := parseFlexible(fs, rest); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	home, err := opts.home()
	if err != nil {
		return usageError("%v", err)
	}

	switch sub {
	case "list":
		return modelListCommand(nil)

	case "remove", "rm":
		ids := fs.Args()
		if len(ids) != 1 {
			return usageError("用法: dsh-gate model provider remove <服务商 id>")
		}
		path, removed, err := dshsettings.RemoveProvider(home, ids[0])
		if err != nil {
			return fatal("删除服务商失败: %v", err)
		}
		if !removed {
			fmt.Printf("设置文件 %s 里没有 %s，无需删除。\n", path, ids[0])
			return 0
		}
		fmt.Printf("已从 %s 删除服务商 %s\n", path, ids[0])
		printModelReloadHint(home)
		return 0

	case "add", "set":
		ids := fs.Args()
		if len(ids) != 1 {
			return usageError("用法: dsh-gate model provider add <服务商 id> --base-url <地址> [--api …] [--key-env …] [--models a,b]")
		}
		if strings.TrimSpace(baseURL) == "" {
			return usageError("需要 --base-url：例如 --base-url https://api.example.com/v1")
		}
		provider := dshsettings.Provider{
			ID:          ids[0],
			DisplayName: showName,
			API:         api,
			BaseURL:     baseURL,
			KeyEnv:      keyEnv,
		}
		for _, item := range strings.Split(modelsFlag, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			id, name := item, ""
			if idx := strings.IndexAny(item, ":="); idx > 0 {
				id, name = strings.TrimSpace(item[:idx]), strings.TrimSpace(item[idx+1:])
			}
			provider.Models = append(provider.Models, dshsettings.Model{ID: id, Name: name})
		}
		path, err := dshsettings.SetProvider(home, provider)
		if err != nil {
			return fatal("写入服务商失败: %v", err)
		}
		fmt.Printf("已写入 %s：%s → %s\n", path, provider.ID, provider.BaseURL)
		if provider.KeyEnv != "" {
			fmt.Printf("密钥：环境变量名 %s（值用 dsh-gate model key set %s --stdin 写入）\n",
				provider.KeyEnv, provider.KeyEnv)
		}
		printModelReloadHint(home)
		return 0

	default:
		return usageError("未知的 model provider 子命令 %q，可用：add / remove / list", sub)
	}
}

// modelDefaultCommand 设置「新建会话默认用哪个服务商与模型」。
func modelDefaultCommand(args []string) int {
	fs := flag.NewFlagSet("model default", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opts modelOptions
	registerModelFlags(fs, &opts)
	if err := parseFlexible(fs, args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	names := fs.Args()
	if len(names) != 2 {
		return usageError("用法: dsh-gate model default <服务商> <模型>\n例如: dsh-gate model default deepseek-official deepseek-chat")
	}
	home, err := opts.home()
	if err != nil {
		return usageError("%v", err)
	}
	path, err := dshsettings.SetDefaultModel(home, names[0], names[1])
	if err != nil {
		return fatal("写入默认模型失败: %v", err)
	}
	fmt.Printf("已写入 %s：默认模型 = %s / %s\n", path, names[0], names[1])
	if names[0] == dshsettings.DeepSeekOfficial {
		if has, _ := dshsettings.HasKey(home, dshsettings.DeepSeekKeyEnv); !has {
			fmt.Printf("\n提醒：还没写入官方密钥，先执行：\n  dsh-gate model key set %s --stdin\n",
				dshsettings.DeepSeekKeyEnv)
		}
	}
	printModelReloadHint(home)
	return 0
}

// printModelReloadHint 说明何时生效、以及备份在哪。
func printModelReloadHint(dshHome string) {
	settingsPath, credentialsPath := dshsettings.Paths(dshHome)
	fmt.Println("提示：DSH 在下次请求时读到新设置；没生效就重启该实例（dsh-gate dsh restart <实例名>）。")
	var backups []string
	for _, path := range []string{settingsPath, credentialsPath} {
		if fileExists(dshsettings.BackupPath(path)) {
			backups = append(backups, dshsettings.BackupPath(path))
		}
	}
	if len(backups) > 0 {
		fmt.Printf("      改前的文件已备份：%s\n", strings.Join(backups, "、"))
	}
}
