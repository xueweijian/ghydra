//go:build linux

// Linux 实现：gsettings（GNOME 系桌面通吃）。无桌面环境时返回带
// 引导的 ApplyError（R6 降级：M1 验收平台是 Windows，Linux 只冒烟）。
package sysproxy

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func linuxDesktop() bool {
	if os.Getenv("GNOME_DESKTOP_SESSION_ID") != "" || os.Getenv("XDG_CURRENT_DESKTOP") != "" {
		if _, err := exec.LookPath("gsettings"); err == nil {
			return true
		}
	}
	return false
}

func supportedOS() bool { return linuxDesktop() }

func currentOS() (Setting, error) {
	if !linuxDesktop() {
		return Setting{}, &ApplyError{Op: "Current", Err: fmt.Errorf("无 GNOME 桌面环境")}
	}
	var s Setting
	out, err := exec.Command("gsettings", "get", "org.gnome.system.proxy", "mode").Output()
	if err != nil {
		return Setting{}, fmt.Errorf("gsettings: %w", err)
	}
	if strings.TrimSpace(string(out)) == "'auto'" {
		if u, err := exec.Command("gsettings", "get", "org.gnome.system.proxy", "autoconfig-url").Output(); err == nil {
			s.PACURL = strings.Trim(strings.TrimSpace(string(u)), "'")
		}
		return s, nil
	}
	if strings.TrimSpace(string(out)) == "'manual'" {
		h, _ := exec.Command("gsettings", "get", "org.gnome.system.proxy.https", "host").Output()
		p, _ := exec.Command("gsettings", "get", "org.gnome.system.proxy.https", "port").Output()
		host := strings.Trim(strings.TrimSpace(string(h)), "'")
		port := strings.TrimSpace(string(p))
		if host != "" && port != "" {
			s.ProxyServer = fmt.Sprintf("%s:%s", host, port)
		}
	}
	return s, nil
}

func applyOS(s Setting) error {
	if !linuxDesktop() {
		return &ApplyError{
			Op:   "Apply",
			Err:  fmt.Errorf("无 GNOME 桌面环境"),
			Hint: "手动接管引导：export http_proxy=http://<addr> https_proxy=http://<addr>，或安装 gsettings",
		}
	}
	if s.PACURL != "" {
		if out, err := exec.Command("gsettings", "set", "org.gnome.system.proxy", "autoconfig-url", s.PACURL).CombinedOutput(); err != nil {
			return fmt.Errorf("autoconfig-url: %v (%s)", err, out)
		}
		_, err := exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "auto").Output()
		return err
	}
	host, port := splitAddr(s.ProxyServer)
	for _, schema := range []string{"http", "https"} {
		exec.Command("gsettings", "set", fmt.Sprintf("org.gnome.system.proxy.%s", schema), "host", host).Run()
		exec.Command("gsettings", "set", fmt.Sprintf("org.gnome.system.proxy.%s", schema), "port", port).Run()
	}
	_, err := exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "manual").Output()
	return err
}

func clearOS() error {
	if !linuxDesktop() {
		return nil // 无桌面=从未接管，清无可清
	}
	_, err := exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", "none").Output()
	return err
}

func splitAddr(addr string) (host, port string) {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i], addr[i+1:]
	}
	return addr, "80"
}
