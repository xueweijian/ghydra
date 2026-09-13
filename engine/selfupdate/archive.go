package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// unpackArchive 解包平台 archive 到 staging 根，仅提取 names 白名单内的
// 常规文件（zip-slip / symlink / 设备文件全拒——解包面也是攻击面）。
// 格式按扩展名：.zip / .tar.gz（其余报错）。
func unpackArchive(arcPath, stagingDir string, names []string) error {
	allow := map[string]bool{}
	for _, n := range names {
		allow[n] = true
	}
	switch {
	case strings.HasSuffix(arcPath, ".zip"):
		return unpackZip(arcPath, stagingDir, allow)
	case strings.HasSuffix(arcPath, ".tar.gz") || strings.HasSuffix(arcPath, ".tgz"):
		return unpackTarGz(arcPath, stagingDir, allow)
	default:
		return fmt.Errorf("selfupdate: 不支持的 archive 格式 %s", path.Base(arcPath))
	}
}

func safeJoin(base, name string) (string, error) {
	clean := path.Clean("/" + name) // 绝对化后 Clean：消灭 ../ 与前导 /
	if clean == "/" || strings.Contains(clean, "\\") {
		return "", fmt.Errorf("非法条目 %q", name)
	}
	return path.Join(base, clean), nil
}

func extractOne(dst string, r io.Reader, mode os.FileMode) error {
	if err := os.WriteFile(dst, []byte{}, mode); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	// 上限由 archive 级 sha256 兜底（archive 本身 ≤ MaxAssetSize），
	// 解包单文件不再重复设限——白名单文件名 + 解压炸弹在源头尺寸顶内。
	_, err = io.Copy(f, r)
	return err
}

func unpackZip(arcPath, stagingDir string, allow map[string]bool) error {
	zr, err := zip.OpenReader(arcPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !allow[path.Base(f.Name)] || path.Dir(path.Clean("/"+f.Name)) != "/" {
			return fmt.Errorf("selfupdate: archive 内白名单外/嵌套路径 %q", f.Name)
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("selfupdate: archive 含 symlink %q", f.Name)
		}
		dst, err := safeJoin(stagingDir, f.Name)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		werr := extractOne(dst, rc, f.Mode().Perm())
		rc.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}

func unpackTarGz(arcPath, stagingDir string, allow map[string]bool) error {
	f, err := os.Open(arcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			if !allow[path.Base(hdr.Name)] || path.Dir(path.Clean("/"+hdr.Name)) != "/" {
				return fmt.Errorf("selfupdate: archive 内白名单外/嵌套路径 %q", hdr.Name)
			}
			dst, err := safeJoin(stagingDir, hdr.Name)
			if err != nil {
				return err
			}
			perm := os.FileMode(hdr.Mode) & 0o777
			if perm&0o100 == 0 { // 保底可执行（主程序）
				perm |= 0o755
			}
			if err := extractOne(dst, tr, perm); err != nil {
				return err
			}
		case tar.TypeDir:
			continue
		default:
			return fmt.Errorf("selfupdate: archive 含非常规条目 %q (type %c)", hdr.Name, hdr.Typeflag)
		}
	}
}
