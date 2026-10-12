package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wycto/dsh-gate/internal/config"
	"github.com/wycto/dsh-gate/internal/term"
	"github.com/wycto/dsh-gate/internal/users"
)

// userCommand 实现 `dsh-gate user`。
func userCommand(args []string) int {
	if len(args) == 0 {
		printUserHelp(os.Stderr)
		return 2
	}
	switch args[0] {
	case "add":
		return userAdd(args[1:])
	case "passwd":
		return userPasswd(args[1:])
	case "list":
		return userList(args[1:])
	case "rm", "remove":
		return userRemove(args[1:])
	case "-h", "--help", "help":
		printUserHelp(os.Stdout)
		return 0
	default:
		return usageError("未知的 user 子命令 %q，可用：add / passwd / list / rm", args[0])
	}
}

func printUserHelp(w *os.File) {
	fmt.Fprint(w, `用法: dsh-gate user <add|passwd|list|rm> [选项]

  add <用户名>     添加或修改账号
  passwd <用户名>  修改已有账号的口令（账号不存在时拒绝，不会误建新号）
  list             列出账号
  rm <用户名>      删除账号

创建第一个账号:
  dsh-gate user add admin

修改某个账号的口令（忘记旧口令也能改）:
  dsh-gate user passwd admin

管理某个实例的账号（它用了单独的状态目录或账号表时，带上同一个选项）:
  dsh-gate user list --state-dir ~/.dsh-gate-company

脚本 / CI 里设置口令:
  echo "$PASSWORD" | dsh-gate user add admin --password-stdin

口令改动对运行中的网关立即生效，无需重启。

选项:
`)
}

// parseUserFlags 在 fs 上注册账号管理共用的选项、解析命令行，并返回账号表路径。
//
// 账号表路径由 --users 或 --state-dir 推导（<状态目录>/users.json）；实例用了
// 单独的 --state-dir / --users，管理它的账号就要带上同一个选项，才会指到同一
// 份表。--dsh-home 也一并接受，但它只属于 run 的实例配置，与账号表位置无关
// （账号表刻意不放进 dsh 的实例目录，见 config 包的说明）。
func parseUserFlags(fs *flag.FlagSet, args []string) (string, error) {
	fs.String("users", "", "账号表路径（留空则放在状态目录里）")
	fs.String("state-dir", "", "网关状态目录，用于推导账号表路径")
	fs.String("dsh-home", "", "dsh 实例目录（只影响 dsh 本身，不影响账号表位置）")

	if err := parseFlexible(fs, args); err != nil {
		return "", err
	}

	cfg := config.Default()
	if err := config.ApplyEnv(&cfg, os.Getenv); err != nil {
		return "", err
	}
	// 只把这两个选项转成配置键：同一个 FlagSet 上还有 --random、
	// --iterations 这类账号子命令自己的开关，它们不参与实例配置。
	set := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "users", "state-dir", "dsh-home":
			set[f.Name] = f.Value.String()
		}
	})
	if err := config.Apply(&cfg, set); err != nil {
		return "", err
	}
	if err := cfg.Normalize(); err != nil {
		return "", err
	}
	return cfg.UsersFile, nil
}

// userAdd 添加或修改账号。
func userAdd(args []string) int {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	iterations := fs.Int("iterations", users.DefaultIterations, "PBKDF2 迭代次数（调高更安全也更慢）")
	randomMode := fs.Bool("random", false, "自动生成一个强随机口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令（适合脚本）")

	usersPath, err := parseUserFlags(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageError("请给出用户名：dsh-gate user add <用户名>")
	}
	name := fs.Arg(0)
	if err := users.ValidateName(name); err != nil {
		return usageError("%v", err)
	}

	password, err := acquirePassword(*randomMode, *fromStdin)
	if err != nil {
		return fatal("%v", err)
	}
	if err := users.ValidatePassword(password); err != nil {
		return usageError("%v", err)
	}

	store, err := users.Open(usersPath)
	if err != nil {
		return fatal("%v", err)
	}
	existed := store.Exists(name)
	if err := store.Set(name, password, *iterations); err != nil {
		return fatal("%v", err)
	}

	verb := "创建"
	if existed {
		verb = "更新"
	}
	fmt.Printf("已%s账号 %q，当前共 %d 个 -> %s\n", verb, name, store.Count(), usersPath)
	if *randomMode {
		fmt.Println()
		fmt.Println("  初始口令（只显示这一次，请立即保存）：")
		fmt.Println("    " + password)
	}
	return 0
}

// userPasswd 修改已有账号的口令。
//
// 与 user add 的区别在语义：add 是「没有就建，有就覆盖」，passwd 只更新
// 已存在的账号——用户名打错时它当场失败，而不是悄悄多出一个新账号。
// 忘记旧口令也能改：账号表里只有哈希，校验旧口令既不可能、也不必要。
func userPasswd(args []string) int {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	iterations := fs.Int("iterations", users.DefaultIterations, "PBKDF2 迭代次数（调高更安全也更慢）")
	randomMode := fs.Bool("random", false, "自动生成一个强随机口令并打印一次")
	fromStdin := fs.Bool("password-stdin", false, "从标准输入读取口令（适合脚本）")

	usersPath, err := parseUserFlags(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageError("请给出用户名：dsh-gate user passwd <用户名>")
	}
	name := fs.Arg(0)

	store, err := users.Open(usersPath)
	if err != nil {
		return fatal("%v", err)
	}
	// 先确认账号存在再问口令：别让人输完两遍口令才被告知名字打错了。
	if !store.Exists(name) {
		return fatal("账号 %q 不存在；要新建账号请用 dsh-gate user add %s", name, name)
	}

	password, err := acquirePassword(*randomMode, *fromStdin)
	if err != nil {
		return fatal("%v", err)
	}
	if err := users.ValidatePassword(password); err != nil {
		return usageError("%v", err)
	}
	if err := store.Set(name, password, *iterations); err != nil {
		return fatal("%v", err)
	}

	fmt.Printf("已更新账号 %q 的口令 -> %s\n", name, usersPath)
	if *randomMode {
		fmt.Println()
		fmt.Println("  新口令（只显示这一次，请立即保存）：")
		fmt.Println("    " + password)
	}
	return 0
}

// acquirePassword 取得要设置的口令。
//
// 三条路，按「人怎么用」来分：交互输入（默认）、管道（脚本 / CI）、自动生成
// （懒得想口令，或给一次性账号）。交互输入要求输两遍，且不回显。
func acquirePassword(randomMode, fromStdin bool) (string, error) {
	switch {
	case randomMode:
		return randomPassword(), nil

	case fromStdin:
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return "", fmt.Errorf("读取标准输入失败: %w", err)
		}
		password := strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			return "", fmt.Errorf("标准输入里没有口令")
		}
		return password, nil
	}

	if !term.IsTerminal(os.Stdin.Fd()) {
		return "", fmt.Errorf(`当前不是交互式终端，无法安全地输入口令。
改用以下任一方式:
  dsh-gate user add <用户名> --password-stdin < 口令文件
  dsh-gate user add <用户名> --random`)
	}

	first, err := term.ReadPassword(os.Stdin, os.Stderr, "请输入口令: ")
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	second, err := term.ReadPassword(os.Stdin, os.Stderr, "请再输入一次: ")
	if err != nil {
		return "", fmt.Errorf("读取口令失败: %w", err)
	}
	if string(first) != string(second) {
		return "", fmt.Errorf("两次输入的口令不一致")
	}
	return string(first), nil
}

// randomPassword 生成一个便于手抄的随机口令。
func randomPassword() string {
	// 去掉 0/O/1/l/I 这类易混字符，再取 20 位：熵依然远超任何口令字典，
	// 同时不至于长到没法粘贴到手机上。
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		// 随机源不可用时直接失败，而不是退化成可预测的口令。
		panic("读取系统随机数失败，无法生成口令: " + err.Error())
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// userList 列出账号。
func userList(args []string) int {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usersPath, err := parseUserFlags(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageError("%v", err)
	}
	store, err := users.Open(usersPath)
	if err != nil {
		return fatal("%v", err)
	}
	names := store.Names()
	if len(names) == 0 {
		fmt.Printf("账号表 %s 里还没有账号。\n创建一个：dsh-gate user add admin\n", usersPath)
		return 0
	}
	fmt.Printf("账号表 %s（%d 个）:\n", usersPath, len(names))
	for _, name := range names {
		record, _ := store.Get(name)
		fmt.Printf("  %-24s 创建于 %s\n", name, record.CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return 0
}

// userRemove 删除账号。
func userRemove(args []string) int {
	fs := flag.NewFlagSet("user rm", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	usersPath, err := parseUserFlags(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageError("%v", err)
	}
	if fs.NArg() != 1 {
		return usageError("请给出要删除的用户名：dsh-gate user rm <用户名>")
	}
	store, err := users.Open(usersPath)
	if err != nil {
		return fatal("%v", err)
	}
	name := fs.Arg(0)
	if err := store.Remove(name); err != nil {
		return fatal("%v", err)
	}
	fmt.Printf("已删除账号 %q，当前共 %d 个\n", name, store.Count())
	return 0
}
