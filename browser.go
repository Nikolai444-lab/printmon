package main

import (
	"os/exec"
	"runtime"
	"strings"
)

// openBrowser opens url in the system default browser, in a new tab.
// Запускаем и сразу отпускаем процесс: ждать закрытия браузера программе незачем.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 — самый тихий способ: в варианте без консоли (printmon-gui.exe)
		// «cmd /c start» мигал бы чёрным окном.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// browserURL turns the listen address into something a browser can open:
// 0.0.0.0 и пустой хост на машине владельца означают «этот компьютер».
func browserURL(listen string) string {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		addr = DefaultListen
	}
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		port = addr
		host = ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6
	}
	return "http://" + host + ":" + port
}
