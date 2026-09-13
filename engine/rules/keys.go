package rules

import (
	"crypto/ed25519"
	"sync"
)

// 发布信任锚（M3-W2 设计 §2.2/§6）：公钥编译期冻结进二进制——运行时
// 无法被磁盘/网络替换，这是三级信任地板的根。
//
// 当前为**开发密钥**（key_id 13d7ddfa7c97895c，私钥存维护者离线介质，
// 永不进 repo/CI）。v1.0 发布前轮换为正式密钥：keys.go 变更 = 发版动作，
// 走 PR + CI 全绿 + tag。轮换协议（keys[] 字段）v2.0 启用。

const releasePublicKeyFile = `untrusted comment: minisign public key 5C89977CFADDD713
RWQT1936fJeJXFqXvxaFZEH/YabwPbMYHaEAgg4Plk2GfPUkf5AqNs4S
`

var (
	keyOnce  sync.Once
	keyPub   ed25519.PublicKey
	keyID    [8]byte
	keyError error
)

// ActiveKey 返回当前信任的公钥与 key_id（签名 blob 内的 8 字节真实值，
// 非公钥文件注释行的展示名——两者在该 minisign 实现中不同，见 minisign_test）。
func ActiveKey() (ed25519.PublicKey, [8]byte, error) {
	keyOnce.Do(func() {
		pub, id, err := ParsePublicKey([]byte(releasePublicKeyFile))
		if err != nil {
			keyError = err
			return
		}
		keyPub = pub
		copy(keyID[:], id)
	})
	return keyPub, keyID, keyError
}
