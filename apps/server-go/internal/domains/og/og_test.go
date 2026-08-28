package og

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fetchAndParse 直测(httptest 服不经 SSRF 面——那是 assertPublicHost 的
// 职责,mirror-og.test.ts 覆盖)。
func TestFetchAndParseOGMeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head>
			<meta property="og:title" content="Hello &amp; Welcome">
			<meta property="og:description" content="A &#39;desc&#39; &quot;here&quot;">
			<meta property="og:image" content="/img/hero.png">
			<meta property="og:site_name" content="Example">
			<title>fallback title</title>
		</head><body></body></html>`))
	}))
	defer srv.Close()

	res, err := fetchAndParse(context.Background(), srv.Client(), srv.URL+"/some/page")
	if err != nil {
		t.Fatalf("fetchAndParse: %v", err)
	}
	if res.Title != "Hello & Welcome" {
		t.Errorf("title = %q, want og:title decoded", res.Title)
	}
	if res.Description != `A 'desc' "here"` {
		t.Errorf("description = %q, want entity-decoded og:description", res.Description)
	}
	if res.Image != srv.URL+"/img/hero.png" {
		t.Errorf("image = %q, want relative resolved against final URL", res.Image)
	}
	if res.SiteName != "Example" {
		t.Errorf("siteName = %q", res.SiteName)
	}
}

func TestFetchAndParseFallbackChain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta name="twitter:title" content="tw title">
			<meta name="description" content="plain desc">
			<title>doc title</title>
		</head></html>`))
	}))
	defer srv.Close()

	res, err := fetchAndParse(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetchAndParse: %v", err)
	}
	if res.Title != "tw title" {
		t.Errorf("title = %q, want twitter:title over <title>", res.Title)
	}
	if res.Description != "plain desc" {
		t.Errorf("description = %q, want name=description fallback", res.Description)
	}
}

// property 优先于 name:同键两标签并存时取 property 面(TS pickMetaContent 优先级)。
func TestFetchAndParsePropertyBeatsName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head>
			<meta name="og:title" content="from-name">
			<meta property="og:title" content="from-property">
		</head></html>`))
	}))
	defer srv.Close()

	res, err := fetchAndParse(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("fetchAndParse: %v", err)
	}
	if res.Title != "from-property" {
		t.Errorf("title = %q, want property tag to win regardless of order", res.Title)
	}
}

func TestFetchAndParseUpstreamErrors(t *testing.T) {
	fourOhFour := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))
	defer fourOhFour.Close()
	if _, err := fetchAndParse(context.Background(), fourOhFour.Client(), fourOhFour.URL); err == nil {
		t.Error("404 upstream should error")
	} else if oe, ok := err.(*ogError); !ok || oe.status != http.StatusBadGateway {
		t.Errorf("404 should map to 502 ogError, got %v", err)
	}

	badCT := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer badCT.Close()
	if _, err := fetchAndParse(context.Background(), badCT.Client(), badCT.URL); err == nil {
		t.Error("non-HTML content-type should error")
	} else if oe, ok := err.(*ogError); !ok || oe.status != http.StatusUnsupportedMediaType {
		t.Errorf("content-type should map to 415 ogError, got %v", err)
	}
}

func TestValidateURLString(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://example.com/a?b=1#frag", "https://example.com/a?b=1", false},
		{"http://example.com/", "http://example.com/", false},
		{"ftp://example.com/", "", true},
		{"not a url", "", true},
	}
	for _, c := range cases {
		got, err := validateURLString(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("validateURLString(%q): expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("validateURLString(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("validateURLString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodeEntities(t *testing.T) {
	if got := decodeEntities("&amp;&quot;&#39;&lt;&gt;&nbsp;"); got != `&"'<> ` {
		t.Errorf("basic entities = %q", got)
	}
	if got := decodeEntities("&#233;&#x1F600;"); got != "é😀" {
		t.Errorf("numeric entities = %q", got)
	}
}
