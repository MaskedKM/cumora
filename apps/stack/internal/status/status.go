// status —— "现在什么状态"面(#281):三件套活性 + livez/healthz 探测
// + 栈版本。输出 JSON 形态是阶段 3 桌面管理面的契约面,字段名即契约。
package status

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MaskedKM/cumora/apps/stack/internal/probe"
	"github.com/MaskedKM/cumora/apps/stack/internal/stackd"
)

// UnitReport —— 单个 unit 的状态。Started/Uptime 由
// ExecMainStartTimestamp(systemctl C locale 形态)推导;解析失败时
// Uptime 留空,不猜。
type UnitReport struct {
	Unit    string `json:"unit"`
	Load    string `json:"load"`
	Active  string `json:"active"`
	Sub     string `json:"sub"`
	Started string `json:"started,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ProbeResult —— livez/healthz 类 HTTP 探测。Status 取
// ok/warn/fail:livez 503 = warn(Redis 红的诚实信号,livez 本身
// 活着,重启修不了 —— cumora-go.service 探针注释的语义)。
type ProbeResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Report —— status 全量输出。AnyFail 仅由"连接建立不起来"类失败置位。
// Stackd 段(#282):单 unit 形态下 stackd 的进程内状态快照(状态文件
// 可读即带出;三 unit 形态下为空 —— 两种形态同一 JSON 契约)。
type Report struct {
	Units   []UnitReport     `json:"units"`
	Livez   ProbeResult      `json:"livez"`
	Healthz ProbeResult      `json:"healthz"`
	Version string           `json:"version"`
	Current string           `json:"current"`
	Stackd  *stackd.Snapshot `json:"stackd,omitempty"`
}

// Config —— status 输入(全部可注入,理由同 doctor.Config)。
type Config struct {
	Units       []string
	LivezURL    string
	HealthzURL  string
	SidToken    string // healthz Bearer;缺失时探测仍发(预期 401=活着)
	VersionFile string // releases/<ver>/VERSION 经 current symlink
	CurrentDir  string // current symlink 本体(报告指向)
	StateFile   string // stackd-state.json(可选;单 unit 形态带 stackd 段)
}

// systemd 时间戳形态:"Mon 2026-09-01 09:35:12 CST"(C locale)。
const systemdTimeLayout = "Mon 2006-01-02 15:04:05 MST"

// Run —— 采集状态。只读,单路失败不中断其余采集。
func Run(d probe.Deps, cfg Config) Report {
	var r Report

	for _, u := range cfg.Units {
		st, err := d.Systemd(u)
		ur := UnitReport{Unit: u, Load: st.Load, Active: st.Active, Sub: st.Sub, Started: st.Timestamp}
		if err != nil {
			ur.Error = err.Error()
		} else if t, perr := time.Parse(systemdTimeLayout, st.Timestamp); perr == nil {
			ur.Uptime = time.Since(t).Truncate(time.Second).String()
		}
		r.Units = append(r.Units, ur)
	}

	// livez:200 绿;503 黄(依赖红但探针活着);连不上红。
	if code, err := d.HTTP(cfg.LivezURL, ""); err != nil {
		r.Livez = ProbeResult{"livez", "fail", err.Error()}
	} else {
		switch code {
		case 200:
			r.Livez = ProbeResult{"livez", "ok", "200"}
		case 503:
			r.Livez = ProbeResult{"livez", "warn", "503(依赖红,livez 本身活着)"}
		default:
			r.Livez = ProbeResult{"livez", "warn", fmt.Sprintf("HTTP %d", code)}
		}
	}

	// healthz:Bearer 面,200 与 401 都算活着(unit 探针同语义)。
	if code, err := d.HTTP(cfg.HealthzURL, cfg.SidToken); err != nil {
		r.Healthz = ProbeResult{"healthz", "fail", err.Error()}
	} else {
		switch code {
		case 200, 401:
			detail := fmt.Sprintf("HTTP %d", code)
			if code == 401 {
				detail += "(鉴权面在岗)"
			}
			r.Healthz = ProbeResult{"healthz", "ok", detail}
		default:
			r.Healthz = ProbeResult{"healthz", "warn", fmt.Sprintf("HTTP %d", code)}
		}
	}

	// 栈版本:current/VERSION 内容 + symlink 指向。缺位=warn 语义,但
	// 字段缺省即空串,由消费方判定(桌面面板阶段 3 接手)。
	if data, err := d.ReadFile(cfg.VersionFile); err == nil {
		r.Version = strings.TrimSpace(string(data))
	}
	if target, err := d.Readlink(cfg.CurrentDir); err == nil {
		r.Current = target
	}
	// stackd 段:状态文件读不到(三 unit 形态/stackd 未起)= 留空。
	if cfg.StateFile != "" {
		if data, err := d.ReadFile(cfg.StateFile); err == nil {
			var snap stackd.Snapshot
			if json.Unmarshal(data, &snap) == nil {
				r.Stackd = &snap
			}
		}
	}
	return r
}
