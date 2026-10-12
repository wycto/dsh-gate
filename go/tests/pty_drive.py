#!/usr/bin/env python3
"""在真 PTY 下驱动一个交互式命令，用于验收 setup 的提问流程。

为什么需要它：setup 的交互路径（端口冲突的选择、API Key 的星号回显）只在
**真终端**里才会走到——管道输入会让 term.IsTerminal 为假，程序改走非交互分支，
恰好把要验的代码绕开。这个脚本用 pty.openpty 造一个真 TTY，把 FEED 里的每一行
在「输出安静下来」时喂进去（也就是等提示语出现再答），并完整录下屏幕内容。

用法：
    FEED='a|b|c' python3 pty_drive.py <命令...>
    # FEED 用 | 分隔，空元素表示直接回车

输出：把终端内容写到 stdout。密钥之类的敏感输入应当不出现在其中——这正是它
被用来断言「星号回显而不是明文」的原因。
"""

import os
import pty
import select
import subprocess
import sys
import time

QUIET = 0.35  # 输出安静多久算「提示语打完了」
TIMEOUT = float(os.environ.get("PTY_TIMEOUT", "180"))


def main() -> int:
    argv = sys.argv[1:]
    if not argv:
        print("用法: FEED='...' python3 pty_drive.py <命令...>", file=sys.stderr)
        return 2

    lines = os.environ.get("FEED", "").split("|")

    master, slave = pty.openpty()
    proc = subprocess.Popen(
        argv, stdin=slave, stdout=slave, stderr=slave, close_fds=True
    )
    os.close(slave)

    output = bytearray()
    sent = 0
    last_output = time.time()
    deadline = time.time() + TIMEOUT

    while time.time() < deadline:
        readable, _, _ = select.select([master], [], [], 0.1)
        if readable:
            try:
                chunk = os.read(master, 4096)
            except OSError:
                break
            if not chunk:
                break
            output += chunk
            last_output = time.time()
            continue

        # 输出安静下来、还有没喂的行：喂下一行。
        if sent < len(lines) and time.time() - last_output >= QUIET:
            os.write(master, (lines[sent] + "\n").encode())
            sent += 1
            last_output = time.time()
            continue

        # 行喂完了，进程也结束了：收尾。
        if proc.poll() is not None and sent >= len(lines):
            break

    # 再收一会儿残留输出。
    try:
        while True:
            readable, _, _ = select.select([master], [], [], 0.2)
            if not readable:
                break
            chunk = os.read(master, 4096)
            if not chunk:
                break
            output += chunk
    except OSError:
        pass

    try:
        os.close(master)
    except OSError:
        pass

    if proc.poll() is None:
        proc.kill()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        pass

    sys.stdout.write(output.decode("utf-8", errors="replace"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
