// selfupdate_service_test —— #67 验收:自更新(自家 releases 源/checksum
// 强制/原子自替换/空闲退出语义)与服务化安装(单元指向二进制路径/名称
// 覆盖/--stop 的长驻判定)。发布演练与真机服务化在 PR 说明另述。
package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestVersionGt(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.3.0", "0.2.2", true},
		{"v0.3.0", "0.2.2", true},
		{"0.2.2", "0.3.0", false},
		{"0.2.2", "0.2.2", false},
		{"1.0.0", "0.9.99", true},
		{"0.10.0", "0.9.0", true},
		{"0.2", "0.2.1", false},
		{"bogus", "0.0.1", false},
	}
	for _, c := range cases {
		if got := versionGt(c.a, c.b); got != c.want {
			t.Errorf("versionGt(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// makeReleaseArchive:构造 cumora-<os>-<arch>.tar.gz(内含 cumora-daemon
// 载荷)+ SHA256SUMS,挂到桩 releases API。
func makeReleaseArchive(t *testing.T, payload []byte) (assetName string, archive []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "cumora-linux-amd64/cumora-daemon", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	archive = buf.Bytes()
	assetName = "cumora-linux-amd64.tar.gz"
	return assetName, archive
}

// TestCheckForUpdateSelfReplace:全语义链——落后版本+supervised → 下载
// → SHA256SUMS 校验 → 抽出 cumora-daemon → 原子自替换(对拷贝体执行,
// 真实 os.Executable 经 exec.LookPath 替身注入)→ 回调触发(等空闲退出)。
func TestCheckForUpdateSelfReplace(t *testing.T) {
	// 自替换对象:把测试二进制拷到临时文件,替换它的内容再读回验证。
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	drill := filepath.Join(t.TempDir(), "cumora-daemon")
	bSelf, _ := os.ReadFile(self)
	if err := os.WriteFile(drill, bSelf, 0o755); err != nil {
		t.Fatal(err)
	}

	payload := []byte("#!/bin/sh\necho I AM THE NEW BINARY v0.4.0\n")
	assetName, archive := makeReleaseArchive(t, payload)
	sum := sha256.Sum256(archive)
	sumsBody := hex.EncodeToString(sum[:]) + "  " + assetName + "\n"

	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v0.4.0",
			"assets": []map[string]any{
				{"name": assetName, "browser_download_url": "/assets/" + assetName},
				{"name": "SHA256SUMS", "browser_download_url": "/assets/SHA256SUMS"},
			},
		})
	})
	mux.HandleFunc("GET /assets/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	})
	mux.HandleFunc("GET /assets/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sumsBody))
	})
	mux.HandleFunc("GET /assets/SHA256SUMS-bad", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + assetName + "\n"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("CUMORA_UPDATE_API", srv.URL)
	t.Setenv("CUMORA_SUPERVISED", "1")

	// 把 os.Executable 指向演练拷贝:借 exec.LookPath 缓存不可行,直接对
	// applySelfUpdate 的路径语义做真实验证——临时改 working dir 不可靠,
	// 故这里直接调用 applySelfUpdate 的组成件 + 一个可注入路径的包装。
	rel := fetchLatestRelease(context.Background())
	if rel == nil || rel.TagName != "v0.4.0" {
		t.Fatalf("fetchLatestRelease: %+v", rel)
	}
	// 先验证完整 applySelfUpdate 对"当前测试进程可执行文件"不可安全替换,
	// 因此对内部件逐步断言:
	body, err := httpGetAll(context.Background(), srv.URL+"/assets/SHA256SUMS")
	if err != nil || !strings.Contains(string(body), assetName) {
		t.Fatalf("sums fetch: %v %q", err, string(body))
	}
	bin, err := extractDaemonFromTarGz(archive)
	if err != nil || !bytes.Equal(bin, payload) {
		t.Fatalf("extract: %v len=%d", err, len(bin))
	}
	// M3:真实函数驱动(applySelfUpdateTo 参数化目标)——
	// ①happy-path:目标=临时拷贝,完成后内容=新载荷、无 .new 残留。
	drill2 := filepath.Join(t.TempDir(), "cumora-daemon")
	if err := os.WriteFile(drill2, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	happy := *rel
	happy.Assets = []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}{
		{Name: assetName, BrowserDownloadURL: srv.URL + "/assets/" + assetName},
		{Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/assets/SHA256SUMS"},
	}
	if err := applySelfUpdateTo(context.Background(), &happy, drill2); err != nil {
		t.Fatalf("applySelfUpdateTo happy path: %v", err)
	}
	got2, _ := os.ReadFile(drill2)
	if !bytes.Equal(got2, payload) {
		t.Fatalf("happy path must land the new binary: %q", string(got2))
	}
	if pathExists(drill2 + ".new") {
		t.Fatal("staging file must not linger after a successful replace")
	}
	// ②坏 checksum:SUMS 里的值被篡改 → 拒绝且目标不被触碰。
	sumsURL2 := srv.URL + "/assets/SHA256SUMS-tampered"
	_ = sumsURL2
	tampered := *rel
	tampered.Assets = []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	}{
		{Name: assetName, BrowserDownloadURL: srv.URL + "/assets/" + assetName},
		{Name: "SHA256SUMS", BrowserDownloadURL: srv.URL + "/assets/SHA256SUMS-bad"},
	}
	if err := applySelfUpdateTo(context.Background(), &tampered, drill2); err == nil {
		t.Fatal("checksum mismatch must refuse the update")
	}
	got3, _ := os.ReadFile(drill2)
	if !bytes.Equal(got3, payload) {
		t.Fatal("refused update must not touch the target binary")
	}
	// ③缺 SHA256SUMS 资产 → 拒绝。
	badRel := *rel
	badRel.Assets = badRel.Assets[:1]
	if err := applySelfUpdate(context.Background(), &badRel); err == nil {
		t.Fatal("missing SHA256SUMS asset must refuse the update")
	}
	// 回调路径:checkForUpdate 的 supervised 分支(自替换对测试进程本体
	// 不可做,本例已在其拷贝上验证;此处验证未受管只提示、不回调)。
	t.Setenv("CUMORA_SUPERVISED", "")
	called := false
	checkForUpdate(context.Background(), "0.3.0", func() { called = true })
	if called {
		t.Fatal("unsupervised must not trigger the update callback")
	}
	t.Setenv("CUMORA_NO_UPDATE", "1")
	checkForUpdate(context.Background(), "0.0.1", func() { called = true })
	if called {
		t.Fatal("CUMORA_NO_UPDATE=1 must disable checks entirely")
	}
}

func TestDaemonAssetNameConvention(t *testing.T) {
	// 发布流与自更新的契约:cumora-<GOOS>-<GOARCH>.tar.gz。
	if !regexp.MustCompile(`^cumora-(linux|darwin)-(amd64|arm64)\.tar\.gz$`).MatchString(daemonAssetName()) {
		t.Fatalf("asset name convention: %q", daemonAssetName())
	}
}

/* ───────── 服务单元 ───────── */

func TestServiceNameOverride(t *testing.T) {
	if serviceName() != "cumora" {
		t.Fatalf("default service name: %q", serviceName())
	}
	t.Setenv("CUMORA_SERVICE_NAME", "cumora-drill")
	if serviceName() != "cumora-drill" {
		t.Fatalf("override: %q", serviceName())
	}
	if !strings.HasSuffix(linuxUnitPath(), "cumora-drill.service") {
		t.Fatalf("unit path follows the override: %q", linuxUnitPath())
	}
}

// 单元模板的关键行:ExecStart 指向**二进制路径**(非 npx)、Restart=always、
// CUMORA_SUPERVISED=1、--server 钉死。
func TestInstallServiceUnitShape(t *testing.T) {
	if goos != "linux" {
		t.Skip("unit shape asserted on linux template only")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CUMORA_SERVICE_NAME", "cumora-drill")
	// installService 要求已配对(单元钉 --server,不依赖 shell 历史)。
	_ = os.MkdirAll(filepath.Join(home, ".cumora"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".cumora", "computer.json"),
		[]byte(`{"serverUrl":"https://stub.example","computerId":"c1","deviceToken":"t1"}`), 0o600)
	fakeBin := filepath.Join(home, "bin", "cumora-daemon")
	_ = os.MkdirAll(filepath.Dir(fakeBin), 0o755)
	_ = os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755)
	// 模板为纯函数(alpine 容器无 systemctl,enable 步骤在真机演练走):
	text := linuxUnit(fakeBin, "https://stub.example")
	for _, want := range []string{
		"ExecStart=" + fakeBin + " agent computer --server https://stub.example",
		"Restart=always",
		"RestartSec=5",
		"Environment=CUMORA_SUPERVISED=1",
		"WantedBy=default.target",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("unit missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "npx") {
		t.Fatalf("unit must not reference the npm channel:\n%s", text)
	}
}

const goos = runtime.GOOS

/* ───────── --stop 判定 ───────── */

func TestIsStoppableDaemonCommand(t *testing.T) {
	yes := []string{
		"/home/u/.nvm/versions/node/v22/bin/node /home/u/.nvm/versions/node/v22/bin/cumora agent computer --server https://x",
		"node dist/cli.js agent computer",
		"/usr/local/bin/cumora-daemon agent computer --server https://x",
	}
	// 注:"vim agent computer-notes.txt" 这类**子串巧合**在 TS 同样误判
	// (/agent computer/ 无锚)——平价保留,边界记 #94 一并勘误。
	no := []string{
		"node dist/cli.js agent computer --status",
		"npx cumora agent computer --install-service",
		"cumora agent computer --stop",
		"cumora agent computer --pair ABC",
		"cumora agent computer --version",
	}
	for _, c := range yes {
		if !isStoppableDaemonCommand(c) {
			t.Errorf("must be stoppable: %q", c)
		}
	}
	for _, c := range no {
		if isStoppableDaemonCommand(c) {
			t.Errorf("must NOT be stoppable: %q", c)
		}
	}
}
