#!/usr/bin/env bash
# install.sh 的 dry-run 与 mock 路径断言，外加发布产物文件名契约与 SHA256 校验分支。
# 不访问网络，不编译真正的 mem。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL="$ROOT/scripts/install.sh"
RELEASE="$ROOT/scripts/release-assets.sh"
PUBLISH="$ROOT/scripts/publish-release.sh"
fail=0

say() { printf '%s\n' "$*"; }

fail_case() {
	printf 'FAIL %s\n' "$1" >&2
	if [[ -n "${2:-}" ]]; then
		printf '%s\n' "$2" >&2
	fi
	fail=1
}

# dry KEY=VAL... -- [install.sh args]
# 「--」之前交给 env，之后才是脚本参数（否则 env 会把 --version 当成命令）。
dry() {
	env_args=()
	while [[ $# -gt 0 && "$1" != "--" ]]; do
		env_args+=("$1")
		shift
	done
	if [[ "${1:-}" == "--" ]]; then
		shift
	fi
	if [[ ${#env_args[@]} -eq 0 ]]; then
		bash "$INSTALL" --dry-run "$@"
	else
		env "${env_args[@]}" bash "$INSTALL" --dry-run "$@"
	fi
}

line_is() {
	key="$1"
	want="$2"
	got="$(printf '%s\n' "$3" | sed -n "s/^${key}=//p" | head -1)"
	if [[ "$got" != "$want" ]]; then
		fail_case "$key" "want [$want] got [$got]"
		return 1
	fi
	return 0
}

mock_bin() {
	d="$(mktemp -d)"
	cat >"$d/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${CURL_FORBID:-}" == 1 ]]; then
	echo "curl should not be called" >&2
	exit 99
fi
out=""
url=""
prev=""
for a in "$@"; do
	if [[ "$prev" == "-o" ]]; then
		out="$a"
	fi
	prev="$a"
	case "$a" in
	https://*) url="$a" ;;
	esac
done
if [[ -z "$out" ]]; then
	echo "mock curl: missing -o" >&2
	exit 2
fi
printf '%s\n' "$*" >>"${CURL_LOG:?}"
if [[ "${CURL_FAIL:-}" == 1 ]]; then
	echo "mock curl fail" >&2
	exit 22
fi
if [[ "$url" == *checksums.txt ]]; then
	if [[ "${CURL_SUM_MISSING:-}" == 1 ]]; then
		echo "mock curl: checksums 404" >&2
		exit 22
	fi
	h="$(printf 'MZ-fake-binary\n' | { sha256sum 2>/dev/null || shasum -a 256; } | awk '{print $1}')"
	if [[ "${CURL_SUM_BAD:-}" == 1 ]]; then
		h="0000000000000000000000000000000000000000000000000000000000000000"
	fi
	for n in mem-darwin-amd64 mem-darwin-arm64 mem-linux-amd64 mem-linux-arm64 mem-windows-amd64.exe; do
		printf '%s  %s\n' "$h" "$n"
	done >"$out"
	exit 0
fi
if [[ "${CURL_HTML:-}" == 1 ]]; then
	printf '<html>nope</html>\n' >"$out"
	exit 0
fi
printf 'MZ-fake-binary\n' >"$out"
exit 0
EOF
	cat >"$d/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${GIT_LOG:?}"
	if [[ "$1" != "clone" ]]; then
		echo "unexpected git: $*" >&2
		exit 1
	fi
	dest=""
	for a in "$@"; do
		dest="$a"
	done
	mkdir -p "$dest/cmd/mem"
printf '%s\n' "$dest" >>"${GIT_DEST_LOG:?}"
exit 0
EOF
	cat >"$d/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
{
	printf 'CGO_ENABLED=%s\n' "${CGO_ENABLED-}"
	printf 'GOPROXY=%s\n' "${GOPROXY-}"
	printf 'GOSUMDB=%s\n' "${GOSUMDB-}"
	printf 'pwd=%s\n' "$PWD"
	printf 'args=%s\n' "$*"
} >>"${GO_LOG:?}"
out=""
prev=""
for a in "$@"; do
	if [[ "$prev" == "-o" ]]; then
		out="$a"
	fi
	prev="$a"
done
if [[ -z "$out" ]]; then
	echo "mock go: missing -o" >&2
	exit 2
fi
mkdir -p "$(dirname "$out")"
printf '#!/bin/sh\n' >"$out"
chmod +x "$out"
exit 0
EOF
	cat >"$d/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${GH_LOG:?}"
if [[ "$1" == "release" && "$2" == "view" ]]; then
	if [[ "${GH_RELEASE_EXISTS:-}" == 1 ]]; then
		exit 0
	fi
	exit 1
fi
if [[ "$1" == "release" && ( "$2" == "create" || "$2" == "upload" ) ]]; then
	exit 0
fi
echo "unexpected gh: $*" >&2
exit 1
EOF
	chmod +x "$d/curl" "$d/git" "$d/go" "$d/gh"
	printf '%s\n' "$d"
}

# ---------- dry-run：latest 不能拼成 /releases/download/latest/ ----------
{
	out="$(dry V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64)"
	line_is V2MEM_DRY_ACTION download "$out" || true
	line_is V2MEM_DRY_OS darwin "$out" || true
	line_is V2MEM_DRY_ARCH arm64 "$out" || true
	line_is V2MEM_DRY_BIN mem-darwin-arm64 "$out" || true
	line_is V2MEM_DRY_EXE mem "$out" || true
	line_is V2MEM_DRY_URL "https://github.com/v2camp/v2mem/releases/latest/download/mem-darwin-arm64" "$out" || true
	line_is V2MEM_DRY_SUM checksums.txt "$out" || true
	line_is V2MEM_DRY_SUM_URL "https://github.com/v2camp/v2mem/releases/latest/download/checksums.txt" "$out" || true
	line_is V2MEM_DRY_CLONE_REF main "$out" || true
	line_is V2MEM_DRY_CLONE_URL "https://github.com/v2camp/v2mem.git" "$out" || true
	line_is V2MEM_DRY_SOURCE clone "$out" || true
	if printf '%s\n' "$out" | grep -q '/releases/download/latest/'; then
		fail_case "latest url must not use /releases/download/latest/" "$out"
	fi
	if ! printf '%s\n' "$out" | grep -q '^V2MEM_DRY_MKDIR=mkdir -p '; then
		fail_case "dry-run mkdir" "$out"
	fi
	say "ok dry-run darwin arm64 latest"
}

{
	home_dir="$(mktemp -d)"
	missing="$home_dir/no such/bin"
	out="$(dry V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=x86_64 HOME="$home_dir" -- --version v0.1.0 --dir "$missing")"
	line_is V2MEM_DRY_BIN mem-linux-amd64 "$out" || true
	line_is V2MEM_DRY_URL "https://github.com/v2camp/v2mem/releases/download/v0.1.0/mem-linux-amd64" "$out" || true
	line_is V2MEM_DRY_SUM_URL "https://github.com/v2camp/v2mem/releases/download/v0.1.0/checksums.txt" "$out" || true
	line_is V2MEM_DRY_DIR "$missing" "$out" || true
	if [[ -e "$missing" ]]; then
		fail_case "dry-run must not mkdir" "$missing"
	fi
	say "ok dry-run pinned version does not mkdir"
}

{
	out="$(dry V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=aarch64 -- --from-source --version v1.2.3)"
	line_is V2MEM_DRY_ACTION source "$out" || true
	line_is V2MEM_DRY_BIN mem-linux-arm64 "$out" || true
	line_is V2MEM_DRY_CLONE_REF v1.2.3 "$out" || true
	say "ok dry-run from-source uses tag"
}

{
	out="$(dry V2MEM_INSTALL_OS=mingw64_nt-10.0 V2MEM_INSTALL_ARCH=amd64 --)"
	line_is V2MEM_DRY_OS windows "$out" || true
	line_is V2MEM_DRY_BIN mem-windows-amd64.exe "$out" || true
	line_is V2MEM_DRY_EXE mem.exe "$out" || true
	line_is V2MEM_DRY_URL "https://github.com/v2camp/v2mem/releases/latest/download/mem-windows-amd64.exe" "$out" || true
	say "ok dry-run windows amd64"
}

{
	set +e
	err="$(dry V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=ppc64 -- 2>&1)"
	code=$?
	set -e
	if [[ "$code" -eq 0 ]]; then
		fail_case "unsupported arch should fail" "$err"
	elif ! printf '%s\n' "$err" | grep -q '不支持的架构'; then
		fail_case "unsupported arch message" "$err"
	else
		say "ok unsupported arch"
	fi
}

{
	set +e
	err="$(bash "$INSTALL" --bogus 2>&1)"
	code=$?
	set -e
	if [[ "$code" -ne 2 ]]; then
		fail_case "unknown option exit" "$err"
	else
		say "ok unknown option"
	fi
}

# ---------- 下载成功：先创建目录，不调用 git ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	dest="$dest_root/my bin"
	: >"$logs/curl" 
	: >"$logs/git"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_FAIL=0 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --dir "$dest" --version v9.9.9 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -ne 0 ]]; then
		fail_case "download success exit" "$err"
	elif [[ ! -x "$dest/mem" ]]; then
		fail_case "download success binary" "$err"
	elif [[ -s "$logs/git" ]]; then
		fail_case "download success should not clone" "$(cat "$logs/git")"
	elif ! grep -q 'releases/download/v9.9.9/mem-darwin-arm64' "$logs/curl"; then
		fail_case "curl url" "$(cat "$logs/curl")"
	elif ! grep -q 'checksums.txt' "$logs/curl"; then
		fail_case "should fetch checksums" "$(cat "$logs/curl")"
	elif ! printf '%s\n' "$err" | grep -q 'SHA256 校验通过'; then
		fail_case "verify pass message" "$err"
	else
		say "ok download mkdir, verify and skip clone"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 早期 Release 无校验清单：WARN 降级，仍能安装 ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/curl"
	: >"$logs/git"
	: >"$logs/gitdest"
	: >"$logs/go"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_SUM_MISSING=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --dir "$dest_root/bin" --version v0.5.1 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -ne 0 ]]; then
		fail_case "missing checksums exit" "$err"
	elif [[ ! -x "$dest_root/bin/mem" ]]; then
		fail_case "missing checksums binary" "$err"
	elif ! printf '%s\n' "$err" | grep -q '跳过完整性校验'; then
		fail_case "missing checksums warn" "$err"
	elif [[ -s "$logs/git" ]]; then
		fail_case "missing checksums should not clone" "$(cat "$logs/git")"
	else
		say "ok missing checksums degrades to warn"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 校验不符：拒绝落地坏二进制，回退源码构建 ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/curl"
	: >"$logs/git"
	: >"$logs/gitdest"
	: >"$logs/go"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_SUM_BAD=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --dir "$dest_root/bin" --version v9.9.9 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -ne 0 ]]; then
		fail_case "bad checksum exit" "$err"
	elif ! printf '%s\n' "$err" | grep -q 'SHA256 校验失败'; then
		fail_case "bad checksum message" "$err"
	elif [[ ! -s "$logs/go" ]]; then
		fail_case "bad checksum should fall back to source" "$err"
	elif [[ ! -f "$dest_root/bin/mem" ]]; then
		fail_case "fallback binary missing" "$err"
	elif grep -q 'MZ-fake-binary' "$dest_root/bin/mem"; then
		fail_case "bad binary must not land" "$err"
	else
		say "ok bad checksum rejects binary and falls back"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 下载失败：克隆到临时目录，尊重 GOPROXY，不在仓库根 go build ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/curl"
	: >"$logs/git"
	: >"$logs/gitdest"
	: >"$logs/go"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_FAIL=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			GOPROXY="https://goproxy.cn,direct" GOSUMDB="sum.golang.google.cn" \
			V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=amd64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --dir "$dest_root/bin" --version v3.3.3 2>&1
	)"
	code=$?
	set -e
	clone_line="$(cat "$logs/git")"
	go_log="$(cat "$logs/go")"
	clone_dest="$(cat "$logs/gitdest")"
	go_pwd="$(printf '%s\n' "$go_log" | sed -n 's/^pwd=//p')"
	if [[ "$code" -ne 0 ]]; then
		fail_case "fallback exit" "$err"
	elif ! printf '%s\n' "$clone_line" | grep -q -- '--depth 1 --branch v3.3.3'; then
		fail_case "clone ref" "$clone_line"
	elif [[ "$go_pwd" != "$clone_dest" ]]; then
		fail_case "go build cwd" "pwd=$go_pwd dest=$clone_dest"
	elif [[ "$go_pwd" == "$ROOT" ]]; then
		fail_case "go build must not use repo cwd" "$go_pwd"
	elif ! printf '%s\n' "$go_log" | grep -q 'CGO_ENABLED=0'; then
		fail_case "CGO_ENABLED" "$go_log"
	elif ! printf '%s\n' "$go_log" | grep -q 'GOPROXY=https://goproxy.cn,direct'; then
		fail_case "GOPROXY passthrough" "$go_log"
	elif ! printf '%s\n' "$go_log" | grep -q 'GOSUMDB=sum.golang.google.cn'; then
		fail_case "GOSUMDB passthrough" "$go_log"
	elif ! printf '%s\n' "$go_log" | grep -q -- '-o '"$dest_root/bin/mem"; then
		fail_case "go -o" "$go_log"
	elif [[ ! -x "$dest_root/bin/mem" ]]; then
		fail_case "fallback binary missing" "$err"
	else
		say "ok download failure clones then builds"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- HTML 错误页不当成二进制 ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/curl"
	: >"$logs/git"
	: >"$logs/gitdest"
	: >"$logs/go"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_HTML=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --dir "$dest_root/bin" 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -ne 0 ]]; then
		fail_case "html fallback exit" "$err"
	elif ! grep -q -- '--branch main' "$logs/git"; then
		fail_case "html fallback clone main" "$(cat "$logs/git")"
	elif ! printf '%s\n' "$err" | grep -q '不是可执行文件'; then
		fail_case "html message" "$err"
	else
		say "ok html body falls back to clone"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- --from-source 不调用 curl；latest 克隆 main ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/curl"
	: >"$logs/git"
	: >"$logs/gitdest"
	: >"$logs/go"
	set +e
	err="$(
		CURL_LOG="$logs/curl" CURL_FORBID=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --from-source --dir "$dest_root/bin" 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -ne 0 ]]; then
		fail_case "from-source exit" "$err"
	elif ! grep -q -- '--branch main' "$logs/git"; then
		fail_case "from-source clone main" "$(cat "$logs/git")"
	elif [[ ! -x "$dest_root/bin/mem" ]]; then
		fail_case "from-source binary" "$err"
	else
		say "ok from-source skips curl"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 无 Go：失败信息含安装地址与 goproxy，且不调用 git ----------
{
	mocks="$(mock_bin)"
	rm -f "$mocks/go"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/git"
	bash_bin="$(command -v bash)"
	set +e
	err="$(
		GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" CURL_LOG="$logs/curl" CURL_FORBID=1 \
			V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 \
			PATH="$mocks" \
			"$bash_bin" "$INSTALL" --from-source --dir "$dest_root/bin" 2>&1
	)"
	code=$?
	set -e
	if [[ "$code" -eq 0 ]]; then
		fail_case "missing go should fail" "$err"
	elif ! printf '%s\n' "$err" | grep -q '未找到 go'; then
		fail_case "missing go message" "$err"
	elif ! printf '%s\n' "$err" | grep -q 'goproxy.cn'; then
		fail_case "missing go goproxy hint" "$err"
	elif ! printf '%s\n' "$err" | grep -q 'https://go.dev/dl/'; then
		fail_case "missing go install link" "$err"
	elif [[ -s "$logs/git" ]]; then
		fail_case "missing go should not clone" "$(cat "$logs/git")"
	else
		say "ok missing go"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 未设置 GOPROXY 时给出提示，且不替用户写入 ----------
{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dest_root="$(mktemp -d)"
	: >"$logs/go"
	: >"$logs/git"
	: >"$logs/gitdest"
	set +e
	err="$(
		env -u GOPROXY -u GOSUMDB \
			CURL_LOG="$logs/curl" CURL_FORBID=1 GIT_LOG="$logs/git" GIT_DEST_LOG="$logs/gitdest" GO_LOG="$logs/go" \
			V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=amd64 \
			PATH="$mocks:$PATH" \
			bash "$INSTALL" --from-source --dir "$dest_root/bin" 2>&1
	)"
	code=$?
	set -e
	go_log="$(cat "$logs/go")"
	if [[ "$code" -ne 0 ]]; then
		fail_case "unset proxy exit" "$err"
	elif ! printf '%s\n' "$err" | grep -q 'goproxy.cn'; then
		fail_case "unset proxy hint" "$err"
	elif ! printf '%s\n' "$go_log" | grep -q '^GOPROXY=$'; then
		fail_case "must not invent GOPROXY" "$go_log"
	else
		say "ok unset GOPROXY hint"
	fi
	rm -rf "$mocks" "$logs" "$dest_root"
}

# ---------- 发布产物文件名与 install.sh / publish-release.sh 一致 ----------
assets="mem-darwin-amd64 mem-darwin-arm64 mem-linux-amd64 mem-linux-arm64 mem-windows-amd64.exe"
for name in $assets checksums.txt; do
	if ! grep -q "$name" "$RELEASE"; then
		fail_case "release-assets missing $name"
	fi
	if ! grep -q "$name" "$PUBLISH"; then
		fail_case "publish-release missing $name"
	fi
done
if ! grep -q 'checksums.txt' "$INSTALL"; then
	fail_case "install.sh missing checksums.txt"
fi
{
	out="$(dry V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=amd64 --)"
	line_is V2MEM_DRY_BIN mem-darwin-amd64 "$out" || true
	out="$(dry V2MEM_INSTALL_OS=darwin V2MEM_INSTALL_ARCH=arm64 --)"
	line_is V2MEM_DRY_BIN mem-darwin-arm64 "$out" || true
	out="$(dry V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=amd64 --)"
	line_is V2MEM_DRY_BIN mem-linux-amd64 "$out" || true
	out="$(dry V2MEM_INSTALL_OS=linux V2MEM_INSTALL_ARCH=arm64 --)"
	line_is V2MEM_DRY_BIN mem-linux-arm64 "$out" || true
	out="$(dry V2MEM_INSTALL_OS=windows V2MEM_INSTALL_ARCH=amd64 --)"
	line_is V2MEM_DRY_BIN mem-windows-amd64.exe "$out" || true
	say "ok asset name contract"
}

# ---------- publish-release：缺文件失败；齐全时 create / upload ----------
{
	set +e
	err="$(bash "$PUBLISH" "$ROOT/dist-does-not-exist" v0.0.0 2>&1)"
	code=$?
	set -e
	if [[ "$code" -eq 0 ]]; then
		fail_case "publish missing dir should fail" "$err"
	else
		say "ok publish rejects missing assets"
	fi
}

{
	mocks="$(mock_bin)"
	logs="$(mktemp -d)"
	dist="$(mktemp -d)"
	for name in $assets; do
		printf 'x' >"$dist/$name"
	done
	printf 'x' >"$dist/checksums.txt"
	: >"$logs/gh"
	set +e
	err="$(GH_LOG="$logs/gh" GH_RELEASE_EXISTS=0 PATH="$mocks:$PATH" bash "$PUBLISH" "$dist" v0.9.9 2>&1)"
	code=$?
	set -e
	gh_log="$(cat "$logs/gh")"
	if [[ "$code" -ne 0 ]]; then
		fail_case "publish create exit" "$err"
	elif ! printf '%s\n' "$gh_log" | grep -q 'release create v0.9.9'; then
		fail_case "publish create" "$gh_log"
	elif ! printf '%s\n' "$gh_log" | grep -q 'checksums.txt'; then
		fail_case "publish create checksums asset" "$gh_log"
	else
		for name in $assets; do
			if ! printf '%s\n' "$gh_log" | grep -q "$name"; then
				fail_case "publish create asset $name" "$gh_log"
			fi
		done
		say "ok publish create"
	fi
	: >"$logs/gh"
	set +e
	err="$(GH_LOG="$logs/gh" GH_RELEASE_EXISTS=1 PATH="$mocks:$PATH" bash "$PUBLISH" "$dist" v0.9.9 2>&1)"
	code=$?
	set -e
	gh_log="$(cat "$logs/gh")"
	if [[ "$code" -ne 0 ]]; then
		fail_case "publish upload exit" "$err"
	elif ! printf '%s\n' "$gh_log" | grep -q 'release upload v0.9.9'; then
		fail_case "publish upload" "$gh_log"
	elif ! printf '%s\n' "$gh_log" | grep -q -- '--clobber'; then
		fail_case "publish clobber" "$gh_log"
	elif ! printf '%s\n' "$gh_log" | grep -q 'checksums.txt'; then
		fail_case "publish upload checksums asset" "$gh_log"
	else
		say "ok publish upload"
	fi
	rm -rf "$mocks" "$logs" "$dist"
}

if [[ "$fail" -ne 0 ]]; then
	printf 'install tests FAIL\n' >&2
	exit 1
fi
printf 'install tests ok\n'
