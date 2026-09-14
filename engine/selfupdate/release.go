package selfupdate

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/xueweijian/ghydra/engine/rules"
)

// U2/U3/U8 相关哨兵错误（doctor 与 UI 归因用）。
var (
	ErrNoUpdate          = errors.New("selfupdate: 无可用更新")
	ErrPrerelease        = errors.New("selfupdate: 最新为 prerelease，--pre 显式开启后可装")
	ErrNoMinisig         = errors.New("selfupdate: Release 缺 checksums.txt.minisig，不可信（U8）")
	ErrBadVersion        = errors.New("selfupdate: 该版本曾启动失败被回滚，已跳过")
	ErrChecksumMismatch  = errors.New("selfupdate: 资产 sha256 与清单不符")
	ErrForeignAssetURL   = errors.New("selfupdate: 资产 URL 域不在白名单（U3）")
	ErrDraftRelease      = errors.New("selfupdate: draft release 拒绝")
	ErrSizeMismatch      = errors.New("selfupdate: 资产尺寸与 Release 元数据不符（U4）")
	ErrSizeOverLimit     = errors.New("selfupdate: 资产超过尺寸硬顶（U4）")
	ErrStagingIncomplete = errors.New("selfupdate: staging 文件缺失")
	ErrNoOldToRollback   = errors.New("selfupdate: 无旧版凭证可回滚")
)

// Asset 一个 Release 资产。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release GET /repos/{owner}/{repo}/releases/latest 响应的字段子集。
type Release struct {
	TagName    string  `json:"tag_name"`
	Name       string  `json:"name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	HTMLURL    string  `json:"html_url"`
	Body       string  `json:"body"`
	Assets     []Asset `json:"assets"`
}

// Plan 一次可执行更新的全部要素。
type Plan struct {
	Version   string // 无 v 前缀
	Archive   Asset
	Checksums Asset // checksums.txt
	Minisig   Asset // checksums.txt.minisig
	Notes     string
	HTMLURL   string
}

// SelectOpts 选择门槛（拍板 #3 默认跳 prerelease；降级须显式）。
type SelectOpts struct {
	AllowPrerelease bool
	AllowDowngrade  bool
	State           *State // 可选：bad_version 跳过
	// Variant 变体寻址（P4/D7）：""=CLI（前缀 ghydra-<os>-<arch>.）；
	// "gui"（前缀 ghydra-<os>-<arch>-gui.）。gui 二进制不指定 Variant 会
	// 按前缀误选 CLI 归档——交换后 gui 消失（W4p1 遗留）。无对应变体
	// 资产时明确报错，**绝不静默回落**另一变体。
	Variant string
	// TrustedHosts 资产域白名单覆盖（nil = 冻结 github 域集）。
	// 仅供集成测试把 Check 指向 fake Release server；生产代码不得设置。
	TrustedHosts map[string]bool
}

// MaxAssetSize 单资产尺寸硬顶（U4）。
const MaxAssetSize = 100 << 20 // 100 MiB

// ParseLatestRelease 解析 Releases API 响应。draft 整体拒绝（官方 latest
// 不返回 draft，此处防御性再验）。
func ParseLatestRelease(data []byte) (Release, error) {
	var rel Release
	if err := json.Unmarshal(data, &rel); err != nil {
		return Release{}, fmt.Errorf("selfupdate: Release JSON 解析失败: %w", err)
	}
	if rel.Draft {
		return Release{}, ErrDraftRelease
	}
	if rel.TagName == "" {
		return Release{}, errors.New("selfupdate: Release 缺 tag_name")
	}
	return rel, nil
}

// SelectAsset 按平台/版本门槛/信任门槛选择资产，产出可执行 Plan。
// 检查顺序（先便宜后贵）：版本门槛 → prerelease 门槛 → bad_version →
// 资产定位 → minisig 在场（U8）→ 域白名单（U3）→ 尺寸硬顶（U4）。
func SelectAsset(rel Release, goos, goarch, current string, opts SelectOpts) (Plan, error) {
	target, err := ParseVersion(rel.TagName)
	if err != nil {
		return Plan{}, err
	}
	cur, err := ParseVersion(current)
	if err != nil {
		return Plan{}, fmt.Errorf("selfupdate: 当前版本无法解析 %q: %w", current, err)
	}
	if target.Compare(cur) <= 0 && !opts.AllowDowngrade {
		return Plan{}, ErrNoUpdate
	}
	if rel.Prerelease && !opts.AllowPrerelease {
		return Plan{}, ErrPrerelease
	}
	if opts.State != nil && opts.State.IsBad(target.String()) {
		return Plan{}, ErrBadVersion
	}

	plan := Plan{Version: target.String(), Notes: rel.Body, HTMLURL: rel.HTMLURL}
	prefix := "ghydra-" + goos + "-" + goarch + "."
	if opts.Variant != "" {
		switch opts.Variant {
		case "gui":
			prefix = "ghydra-" + goos + "-" + goarch + "-gui."
		default:
			return Plan{}, fmt.Errorf("selfupdate: 未知变体 %q", opts.Variant)
		}
	}
	for _, a := range rel.Assets {
		switch {
		case a.Name == "checksums.txt":
			plan.Checksums = a
		case a.Name == "checksums.txt.minisig":
			plan.Minisig = a
		case plan.Archive.Name == "" && strings.HasPrefix(a.Name, prefix):
			plan.Archive = a
		}
	}
	if plan.Archive.Name == "" {
		if opts.Variant != "" {
			return Plan{}, fmt.Errorf("selfupdate: 无 %s/%s %s 变体资产（不回落默认变体——变体错装=功能消失）",
				goos, goarch, opts.Variant)
		}
		return Plan{}, fmt.Errorf("selfupdate: 无 %s/%s 平台资产", goos, goarch)
	}
	if plan.Checksums.Name == "" || plan.Minisig.Name == "" {
		return Plan{}, ErrNoMinisig
	}
	for _, a := range []Asset{plan.Archive, plan.Checksums, plan.Minisig} {
		if !hostAllowed(a.URL, opts.TrustedHosts) {
			return Plan{}, fmt.Errorf("%w: %s", ErrForeignAssetURL, a.URL)
		}
	}
	for _, a := range []Asset{plan.Archive, plan.Checksums, plan.Minisig} {
		if a.Size > MaxAssetSize {
			return Plan{}, fmt.Errorf("%w: %s %d", ErrSizeOverLimit, a.Name, a.Size)
		}
	}
	return plan, nil
}

// assetHosts 资产与 API 域白名单（U3：源替换攻击面）。
var assetHosts = map[string]bool{
	"github.com":                           true,
	"api.github.com":                       true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// AssetDomainAllowed 资产 URL 信任判定（冻结域集）：https + 精确 host +
// 无端口 + 无 userinfo。
func AssetDomainAllowed(raw string) bool {
	return hostAllowed(raw, nil)
}

func hostAllowed(raw string, override map[string]bool) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if override != nil {
		// 测试专用旁路（fake Release server）：只匹配 host。
		// 生产路径 override 恒为 nil，走下面的冻结域集全量校验。
		return override[u.Hostname()]
	}
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	return assetHosts[u.Hostname()]
}

// ---- 冻结公钥（与 rules 钥匙分离：规则信任 ≠ 二进制信任）----

const releasePublicKeyFile = `untrusted comment: minisign public key F0716070E4E793E7
RWTnk+fkcGBx8NOBMQvzHmERZBZVPJHQQ/EMOFvlALTI2oeg9QK5guGU
`

var (
	frozenKeyOnce sync.Once
	frozenPub     ed25519.PublicKey
	frozenKeyErr  error
)

// publicKeyOverridePath 公钥文件覆盖（**仅冒烟/演练构建**：ldflags 注入
// main 包后由 cmd 装配层调 SetPublicKeyOverride 传入——本沙箱 go1.23.9
// 实测全路径 -X 静默失效，main.* 形式可靠，故走显式注入）。生产为空
// → 用冻结公钥；这是构建期注入（改二进制本身），信任锚语义不受影响。
var publicKeyOverridePath string

// SetPublicKeyOverride 注入公钥文件覆盖（cmd 装配层 init 调用）。
func SetPublicKeyOverride(path string) { publicKeyOverridePath = path }

// FrozenPublicKey release 签名公钥（编译期冻结；私钥离线，同 W2 纪律）。
func FrozenPublicKey() ed25519.PublicKey {
	frozenKeyOnce.Do(func() {
		src := releasePublicKeyFile
		if publicKeyOverridePath != "" {
			b, err := os.ReadFile(publicKeyOverridePath)
			if err != nil {
				frozenKeyErr = err
				return
			}
			src = string(b)
		}
		frozenPub, _, frozenKeyErr = rules.ParsePublicKey([]byte(src))
	})
	if frozenKeyErr != nil {
		panic("selfupdate: release 公钥不可用: " + frozenKeyErr.Error())
	}
	return frozenPub
}
