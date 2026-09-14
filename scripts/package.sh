#!/bin/sh
# package.sh —— 发布产物装配单一事实源（W4p2 D4/D5；本地 + release.yml 通用）。
#
# 用法: scripts/package.sh <version> <outdir> <platform...>
#   platform ∈ linux-amd64 linux-arm64 windows-amd64 windows-amd64-gui
#              darwin-amd64 darwin-arm64
#
# 产物命名契约（selfupdate SelectAsset 前缀 `ghydra-<goos>-<goarch>.`）：
#   ghydra-<goos>-<goarch>.zip|.tar.gz      CLI 变体（selfupdate 可寻址）
#   ghydra-<goos>-<goarch>-gui.zip          GUI 变体（前缀不匹配 "."，互不误选）
# 归档布局（selfupdate Files 白名单，拍平）：
#   unix: ghydra      win: ghydra.exe
# 附属: <outdir>/SHA256SUMS（sha256sum 标准双空格，= checksums.txt 胚）
set -e
cd "$(dirname "$0")/.."

VER="$1"; shift
OUT="$1"; shift
[ -n "$VER" ] && [ -n "$OUT" ] || { echo "usage: $0 <version> <outdir> <platform...>" >&2; exit 2; }
[ $# -gt 0 ] || set -- linux-amd64 linux-arm64 windows-amd64 windows-amd64-gui darwin-amd64 darwin-arm64

export GOTOOLCHAIN=auto GOMAXPROCS=1
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)  # 绝对化（后续 subshell tar/zip 相对路径陷阱）
LDFLAGS="-s -w -X main.Version=${VER#v}"  # strip v（展示层统一加 v——双 v 防线第一道）
# gui 变体构建追加 -X main.buildVariant=gui（D7 变体寻址声明）
gldflags() { case "$1" in *-gui) echo "$LDFLAGS -X main.buildVariant=gui";; *) echo "$LDFLAGS";; esac; }

sums="$OUT/SHA256SUMS"
: > "$sums"

# go build 重试（PRoot/沙箱 go1.25 间歇性 segfault）。
# 坑（实测）：BusyBox ash 在 if 条件上下文里 segfault 子进程的 $? 会读成 0！
# 因此用命令替换取 rc（该上下文 rc=139 正常）+ 产物存在性双判定。
gobuild() {
  want="$1"; shift
  i=1
  while [ $i -le 8 ]; do
    rm -f "$want"
    # if 条件上下文赋值：免疫 set -e（裸赋值 out=$(...) 会把 rc=139 传播给
    # 赋值语句本身，set -e 直接杀脚本——前两轮实测真凶）
    if out=$("$@" 2>&1); then
      rc=0
    else
      rc=$?
    fi
    if [ $rc -eq 0 ] && [ -f "$want" ]; then return 0; fi
    echo "  [gobuild] rc=$rc try#$i: $*" >&2
    i=$((i+1))
  done
  echo "gobuild: build x8 失败: $out" >&2
  return 139
}

for p in "$@"; do
  case "$p" in
    linux-amd64|linux-arm64|darwin-amd64|darwin-arm64)
      goos=${p%%-*}; goarch=${p##*-}; tag=""
      exe=ghydra; ext=zip; arc="tar.gz"
      ;;
    windows-amd64|windows-amd64-gui)
      goos=windows; goarch=amd64; tag=""
      exe=ghydra.exe; ext=zip; arc="zip"
      case "$p" in *-gui) tag="gui"; outname="ghydra-windows-amd64-gui.zip";; *) outname="ghydra-windows-amd64.zip";; esac
      ;;
    *) echo "unknown platform: $p" >&2; exit 2;;
  esac

  [ "$goos" = windows ] || outname="ghydra-$goos-$goarch.$arc"
  if [ -z "$tag" ]; then
    outname="ghydra-$goos-$goarch.$arc"
  fi

  stage=$(mktemp -d)
  export CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch
  if [ "$p" = "windows-amd64-gui" ]; then
    # F11 防回归：//go:embed all:frontend/dist + dist/.gitkeep stub 让空 dist
    # 静默编过（装机白屏）。发布装配前显式断言前端产物在场。
    [ -f internal/guiapp/frontend/dist/index.html ] || {
      echo "FAIL: gui 变体缺前端产物 internal/guiapp/frontend/dist/index.html" >&2
      echo "      先构建: cd internal/guiapp/frontend && npm ci && npm run build" >&2
      exit 1
    }
    # gui 变体：win CGO=0 纯 Go 可交叉；linux/darwin gui 需 CGO（CI 原生），
    # 本脚本仅负责 windows-amd64-gui（其余平台 gui 由 release.yml 原生 job 出）。
    gobuild "$stage/$exe" go build -p 1 -tags gui -trimpath -ldflags="$(gldflags "$p")" -o "$stage/$exe" ./engine/cmd/ghydra
  else
    gobuild "$stage/$exe" go build -p 1 -trimpath -ldflags="$LDFLAGS" -o "$stage/$exe" ./engine/cmd/ghydra
  fi
  unset CGO_ENABLED GOOS GOARCH

  case "$arc" in
    tar.gz) (cd "$stage" && tar czf "$OUT/$outname" "$exe") ;;
    zip)    (cd "$stage" && python3 -c "
import zipfile, sys
z = zipfile.ZipFile(sys.argv[1], 'w', zipfile.ZIP_DEFLATED)
z.write(sys.argv[2], sys.argv[3])
z.close()" "$OUT/$outname" "$exe" "$exe") ;;
  esac
  rm -rf "$stage"

  # 归档布局自检：解包回读，白名单文件必须在根
  chk=$(mktemp -d)
  case "$arc" in
    tar.gz) tar xzf "$OUT/$outname" -C "$chk" ;;
    zip)    python3 -c "
import zipfile, sys
zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" "$OUT/$outname" "$chk" ;;
  esac
  [ -f "$chk/$exe" ] || { echo "FAIL: $outname 缺根级 $exe" >&2; exit 1; }
  rm -rf "$chk"

  (cd "$OUT" && sha256sum "$outname" >> SHA256SUMS)
  echo "OK $outname"
done
echo "=== SHA256SUMS ==="
cat "$sums"
