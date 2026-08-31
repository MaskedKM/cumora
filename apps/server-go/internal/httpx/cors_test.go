package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 平价钉子:对齐 TS index.ts CORS 段的六条语义(允许回显/通配/预检
// 204/同源零影响/外源零头/桌面源内建放行)。
func TestCORSMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	cases := []struct {
		name string
		env  string // CUMORA_CORS_ORIGINS(t.Setenv 注入后重建中间件)
		// 请求
		origin string
		method string
		// 期望
		wantStatus int
		wantAllow  string // Access-Control-Allow-Origin 期望值,空=不得有
		wantNoCORS bool
	}{
		{
			name:       "桌面源内建放行(无需 env)",
			origin:     "app://cumora",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantAllow:  "app://cumora",
		},
		{
			name:       "env 清单内的源回显",
			env:        "https://app.example.com, https://other.example.com",
			origin:     "https://app.example.com",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantAllow:  "https://app.example.com",
		},
		{
			name:       "env 清单外的源零 CORS 头(请求照常透传)",
			env:        "https://app.example.com",
			origin:     "https://evil.example.com",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantNoCORS: true,
		},
		{
			name:       "通配 * 回显星号",
			env:        "*",
			origin:     "https://anything.example.com",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantAllow:  "*",
		},
		{
			name:       "预检 OPTIONS 204 直接收口",
			origin:     "app://cumora",
			method:     http.MethodOptions,
			wantStatus: http.StatusNoContent,
			wantAllow:  "app://cumora",
		},
		{
			name:       "无 Origin(同源/非浏览器)零影响",
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantNoCORS: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("CUMORA_CORS_ORIGINS", tc.env)
			} else {
				t.Setenv("CUMORA_CORS_ORIGINS", "")
			}
			// 中间件构造时求值允许集(t.Setenv 后重建)。
			srv := httptest.NewServer(CORS()(next))
			defer srv.Close()

			req, err := http.NewRequest(tc.method, srv.URL+"/api/auth/me", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()

			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			gotAllow := res.Header.Get("Access-Control-Allow-Origin")
			if tc.wantNoCORS {
				if gotAllow != "" {
					t.Fatalf("must not carry CORS headers, got Allow-Origin %q", gotAllow)
				}
				return
			}
			if gotAllow != tc.wantAllow {
				t.Fatalf("Allow-Origin = %q, want %q", gotAllow, tc.wantAllow)
			}
			if got := res.Header.Get("Access-Control-Allow-Methods"); got != corsMethods {
				t.Fatalf("Allow-Methods = %q, want %q", got, corsMethods)
			}
			if got := res.Header.Get("Access-Control-Allow-Headers"); got != corsHeaders {
				t.Fatalf("Allow-Headers = %q, want %q", got, corsHeaders)
			}
			if tc.method == http.MethodOptions && res.StatusCode != http.StatusNoContent {
				t.Fatalf("preflight must 204, got %d", res.StatusCode)
			}
		})
	}
}
