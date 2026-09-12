//go:build darwin

// macOS 实现：networksetup。作用域取第一个网络服务（Wi-Fi 优先，
// 以太网随后）——覆盖绝大多数开发机；多网卡场景 M2 按 service 枚举。
package sysproxy

import (
	"fmt"
	"os/exec"
	"strings"
)

func networkService() (string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return "", fmt.Errorf("networksetup 不可用: %w", err)
	}
	services := strings.Split(string(out), "\n")
	var wifi, other string
	for _, l := range services {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "*") || strings.Contains(l, "denotes") {
			continue
		}
		if strings.Contains(strings.ToLower(l), "wi-fi") || strings.Contains(l, "Wi-Fi") {
			wifi = l
			break
		}
		if other == "" {
			other = l
		}
	}
	if wifi != "" {
		return wifi, nil
	}
	if other != "" {
		return other, nil
	}
	return "", fmt.Errorf("未找到网络服务")
}

func supportedOS() bool { return true }

func currentOS() (Setting, error) {
	svc, err := networkService()
	if err != nil {
		return Setting{}, err
	}
	var s Setting
	if out, err := exec.Command("networksetup", "-getautoproxyurl", svc).Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "URL: "); ok && v != "" && !strings.Contains(v, "(null)") {
				if enabled := strings.Contains(string(out), "Enabled: Yes"); enabled {
					s.PACURL = v
				}
			}
		}
	}
	// web 优先，web 未启用再看 secure（两者通常同值；单字段表达的
	// 已知限制记录于 W4.5——mac 集成测试不进 CI，真机验收覆盖）
	for _, args := range [][]string{{"-getwebproxy", "-setwebproxy"}, {"-getsecurewebproxy", "-setsecurewebproxy"}} {
		out, err := exec.Command("networksetup", args[0], svc).Output()
		if err != nil {
			continue
		}
		txt := string(out)
		if !strings.Contains(txt, "Enabled: Yes") {
			continue
		}
		for _, l := range strings.Split(txt, "\n") {
			if h, ok := strings.CutPrefix(strings.TrimSpace(l), "Server: "); ok {
				for _, l2 := range strings.Split(txt, "\n") {
					if p, ok2 := strings.CutPrefix(strings.TrimSpace(l2), "Port: "); ok2 {
						s.ProxyServer = fmt.Sprintf("%s:%s", h, p)
						s.ProxyEnabled = true
						break
					}
				}
				break
			}
		}
		if s.ProxyServer != "" {
			break
		}
	}
	// bypass 列表（多行域名 → ";" 分隔，与 Windows ProxyOverride 同构）
	if out, err := exec.Command("networksetup", "-getproxybypassdomains", svc).Output(); err == nil {
		var doms []string
		for _, l := range strings.Split(string(out), "\n") {
			l = strings.TrimSpace(l)
			if l != "" && !strings.Contains(l, "aren't any") {
				doms = append(doms, l)
			}
		}
		s.ProxyOverride = strings.Join(doms, ";")
	}
	return s, nil
}

func applyOS(s Setting) error {
	svc, err := networkService()
	if err != nil {
		return err
	}
	if s.PACURL != "" {
		if out, err := exec.Command("networksetup", "-setautoproxyurl", svc, s.PACURL).CombinedOutput(); err != nil {
			return fmt.Errorf("setautoproxyurl: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		// 关手动代理避免叠加
		exec.Command("networksetup", "-setwebproxystate", svc, "off").Run()
		exec.Command("networksetup", "-setsecurewebproxystate", svc, "off").Run()
		return nil
	}
	if out, err := exec.Command("networksetup", "-setwebproxy", svc, hostOf(s.ProxyServer), portOf(s.ProxyServer)).CombinedOutput(); err != nil {
		return fmt.Errorf("setwebproxy: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("networksetup", "-setsecurewebproxy", svc, hostOf(s.ProxyServer), portOf(s.ProxyServer)).CombinedOutput(); err != nil {
		return fmt.Errorf("setsecurewebproxy: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	// bypass 列表（";" 分隔 → 展开为多参数；空 = 清空列表）
	if doms := splitOverride(s.ProxyOverride); len(doms) > 0 {
		exec.Command("networksetup", append([]string{"-setproxybypassdomains", svc}, doms...)...).Run()
	} else {
		exec.Command("networksetup", "-setproxybypassdomains", svc).Run()
	}
	exec.Command("networksetup", "-setautoproxystate", svc, "off").Run()
	return nil
}

func splitOverride(ov string) []string {
	if ov == "" {
		return nil
	}
	return strings.Split(ov, ";")
}

func clearOS() error {
	svc, err := networkService()
	if err != nil {
		return err
	}
	exec.Command("networksetup", "-setautoproxystate", svc, "off").Run()
	exec.Command("networksetup", "-setwebproxystate", svc, "off").Run()
	exec.Command("networksetup", "-setsecurewebproxystate", svc, "off").Run()
	return nil
}

func hostOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "80"
}
