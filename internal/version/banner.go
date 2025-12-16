package version

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

const banner = `
 ██████╗  ██████╗ ██╗       ██████╗   █████╗  ██████╗
██╔════╝ ██╔════╝ ██║      ██╔═══██╗ ██╔══██╗ ██╔══██╗
██║      ██║      ██║      ██║   ██║ ███████║ ██║  ██║
██║      ██║      ██║      ██║   ██║ ██╔══██║ ██║  ██║
╚██████╗ ╚██████╗ ███████╗ ╚██████╔╝ ██║  ██║ ██████╔╝
 ╚═════╝  ╚═════╝ ╚══════╝  ╚═════╝  ╚═╝  ╚═╝ ╚═════╝
`

const repoURL = "https://github.com/caidaoli/ccLoad"

// ANSI 颜色码
const (
	colorReset  = "\033[0m"
	colorCyan   = "\033[36m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
)

// PrintBanner 打印启动 Banner 和版本信息到 stderr
func PrintBanner() {
	// 检测是否为终端，非终端不输出颜色
	isTTY := term.IsTerminal(int(os.Stderr.Fd()))

	if isTTY {
		fmt.Fprintf(os.Stderr, "%s%s%s", colorCyan, banner, colorReset)
		fmt.Fprintf(os.Stderr, "  %sAPI Load Balancer & Proxy%s\n\n", colorYellow, colorReset)
		fmt.Fprintf(os.Stderr, "%-14s %s%s%s\n", "Version:", colorGreen, Version, colorReset)
		fmt.Fprintf(os.Stderr, "%-14s %s%s%s\n", "Commit:", colorGreen, Commit, colorReset)
		fmt.Fprintf(os.Stderr, "%-14s %s%s%s\n", "Build Time:", colorGreen, BuildTime, colorReset)
		fmt.Fprintf(os.Stderr, "%-14s %s%s%s\n", "Built By:", colorGreen, BuiltBy, colorReset)
		fmt.Fprintf(os.Stderr, "%-14s %s%s%s\n\n", "Repo:", colorBlue, repoURL, colorReset)
	} else {
		fmt.Fprint(os.Stderr, banner)
		fmt.Fprintf(os.Stderr, "  API Load Balancer & Proxy\n\n")
		fmt.Fprintf(os.Stderr, "%-14s %s\n", "Version:", Version)
		fmt.Fprintf(os.Stderr, "%-14s %s\n", "Commit:", Commit)
		fmt.Fprintf(os.Stderr, "%-14s %s\n", "Build Time:", BuildTime)
		fmt.Fprintf(os.Stderr, "%-14s %s\n", "Built By:", BuiltBy)
		fmt.Fprintf(os.Stderr, "%-14s %s\n\n", "Repo:", repoURL)
	}
}
