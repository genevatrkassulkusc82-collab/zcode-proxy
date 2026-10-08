//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// killProcessByName 非 Windows: pkill。
// 目标按平台区分：macOS 桌面客户端是 ZCode.app/Contents/MacOS/ZCode（无 .exe），
// 直接匹配 "ZCode.exe" 在 macOS 上永远扑空。锁定包内可执行文件的完整路径而非
// "ZCode.app" 子串：后者会误杀 argv 里恰好引用该包的无关进程（tail 日志、编辑器
// 会话等），代理自身装在包旁时甚至会匹配到代理自己的命令行。
func killProcessByName(name string) (string, error) {
	target := name
	if runtime.GOOS == "darwin" {
		target = "ZCode.app/Contents/MacOS/ZCode"
	}
	out, err := exec.Command("pkill", "-f", target).CombinedOutput()
	return string(out), err
}

// runHidden 非 Windows 直接执行
func runHidden(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
