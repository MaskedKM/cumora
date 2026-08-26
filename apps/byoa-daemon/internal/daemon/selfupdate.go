// daemon 包 selfupdate —— 自更新(#67):只查自家 GitHub Releases(永
// 不把上游 daemon 拉来对本 server 说话——错谱系事故类别根除)。发现新
// 版:受管(supervised)时下载本平台制品、校验 checksums、**原子自替换
// 二进制**,然后等全部 agent 空闲干净退出,由服务管理器拉起新版;未受管
// 只提示。对齐 daemon.ts checkForUpdate 的节拍(60s 首查、每 6h 复查、
// 30s idle watch),分发源从 npm registry 换成自家 releases。
package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const updateCheckEvery = 6 * time.Hour // 复查间隔(TS UPDATE_CHECK_MS 同值)
const updateFirstCheck = time.Minute   // 启动后首查(TS 同)
const updateIdleWatch = 30 * time.Second

// updateAPIBase:releases 查询端点。默认自家 GitHub;CUMORA_UPDATE_API 可
// 覆盖(测试与私有部署);CUMORA_NO_UPDATE=1 整体退出。
func updateAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("CUMORA_UPDATE_API")); v != "" {
		return v
	}
	return "https://api.github.com/repos/MaskedKM/cumora"
}

func updateDisabled() bool { return os.Getenv("CUMORA_NO_UPDATE") == "1" }

// versionGt:三点数值比较(TS 同实现;非数值段按 0)。
func versionGt(a, b string) bool {
	pa := versionTriple(a)
	pb := versionTriple(b)
	for i := 0; i < 3; i++ {
		if pa[i] > pb[i] {
			return true
		}
		if pa[i] < pb[i] {
			return false
		}
	}
	return false
}

func versionTriple(v string) [3]int {
	var out [3]int
	for i, part := range strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3) {
		if i >= 3 {
			break
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				break
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out
}

// ghRelease:releases/latest 的最小形状。
type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchLatestRelease:GET <base>/releases/latest。best-effort,失败返回 nil。
func fetchLatestRelease(ctx context.Context) *ghRelease {
	url := updateAPIBase() + "/releases/latest"
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil
	}
	var rel ghRelease
	if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&rel) != nil {
		return nil
	}
	return &rel
}

// daemonAssetName:本平台制品名(发布流固定约定:cumora-<os>-<arch>.tar.gz,
// 内含 cumora-server 与 cumora-daemon)。
func daemonAssetName() string {
	return fmt.Sprintf("cumora-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

// checkForUpdate:对照自家 latest;落后时:受管 → 下载/校验/自替换,成功
// 后回调(等空闲退出);未受管 → 提示。绝不因更新打断在飞 turn。
func checkForUpdate(ctx context.Context, currentVersion string, onSupervisedUpdate func()) {
	if updateDisabled() {
		return
	}
	rel := fetchLatestRelease(ctx)
	if rel == nil {
		return // 离线/抖动——下个节拍再试
	}
	latest := rel.TagName
	if latest == "" || !versionGt(latest, currentVersion) {
		return
	}
	if !supervised() {
		slog.Info("[computer] 🆕 cumora " + latest + " available (running " + currentVersion + "). Restart to update, or run `cumora agent computer --install-service` for auto-updates.")
		return
	}
	slog.Info("[computer] 🆕 cumora " + latest + " available (running " + currentVersion + ") — will restart to apply once idle (in-flight turns are never interrupted for an update)")
	if err := applySelfUpdate(ctx, rel); err != nil {
		slog.Warn("[computer] self-update download/replace failed — staying on the current binary", "err", err)
		return
	}
	onSupervisedUpdate()
}

// applySelfUpdate:下载本平台 tar.gz → SHA256SUMS 校验 → 抽出 cumora-daemon
// → 原子替换自身(同目录 .new 暂存 + rename)。replace 后由调用方等空闲
// 干净退出,服务管理器(Restart=always/KeepAlive)拉起的就是新二进制。
func applySelfUpdate(ctx context.Context, rel *ghRelease) error {
	want := daemonAssetName()
	downloadURL := ""
	for _, a := range rel.Assets {
		if a.Name == want {
			downloadURL = a.BrowserDownloadURL
			break
		}
	}
	if downloadURL == "" {
		return fmt.Errorf("release %s has no asset %q for this platform", rel.TagName, want)
	}
	// checksums(与制品同 release 的 SHA256SUMS)。
	sumsURL := ""
	for _, a := range rel.Assets {
		if a.Name == "SHA256SUMS" {
			sumsURL = a.BrowserDownloadURL
			break
		}
	}
	if sumsURL == "" {
		return fmt.Errorf("release %s lacks SHA256SUMS — refusing unverified update", rel.TagName)
	}
	body, err := httpGetAll(ctx, sumsURL)
	if err != nil {
		return err
	}
	wantSum := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == want {
			wantSum = strings.ToLower(fields[0])
			break
		}
	}
	if wantSum == "" {
		return fmt.Errorf("SHA256SUMS has no entry for %s", want)
	}
	archive, err := httpGetAll(ctx, downloadURL)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != wantSum {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", want, hex.EncodeToString(sum[:])[:16]+"…", wantSum[:16]+"…")
	}
	bin, err := extractDaemonFromTarGz(archive)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)
	staging := self + ".new"
	if err := os.WriteFile(staging, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(staging, self); err != nil {
		_ = os.Remove(staging)
		return err
	}
	slog.Info("[computer] self-update staged: binary replaced with " + rel.TagName + " — restarting to apply once idle")
	return nil
}

// extractDaemonFromTarGz:从平台包抽出 cumora-daemon 二进制。
func extractDaemonFromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("archive has no cumora-daemon entry")
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == "cumora-daemon" && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 128<<20))
		}
	}
}

func httpGetAll(ctx context.Context, url string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s → HTTP %d", url, res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 256<<20))
}
