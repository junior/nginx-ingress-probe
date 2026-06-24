// nginx-ingress-probe — a tiny diagnostics page to verify an NGINX (Plus) ingress
// after an upgrade. Deploy it behind the ingress, open it, and it shows the request
// the ingress forwarded, the Kubernetes version, and (optionally) the NGINX Plus API
// data and Prometheus metrics. Single static binary, standard library only.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/index.html
var files embed.FS

var (
	appVersion = "dev"     // overridden via -ldflags at build time
	buildTime  = "unknown" // overridden via -ldflags at build time
	started    = time.Now()
	tmpl       = template.Must(template.ParseFS(files, "templates/index.html"))
)

// ---- data model ----

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type pageData struct {
	Server    serverInfo  `json:"server"`
	Request   requestInfo `json:"request"`
	Pod       []KV        `json:"pod"`
	Facts     []KV        `json:"facts"`
	Kube      kubeInfo    `json:"kubernetes"`
	Plus      plusInfo    `json:"nginx_plus"`
	Prom      promInfo    `json:"prometheus"`
	Demo      bool        `json:"demo"`
	Generated string      `json:"generated"`
}

type serverInfo struct {
	Version  string `json:"version"`
	Build    string `json:"build"`
	Go       string `json:"go"`
	Uptime   string `json:"uptime"`
	Hostname string `json:"hostname"`
}

type headerKV struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Forwarded bool   `json:"forwarded"`
}

type requestInfo struct {
	Method     string     `json:"method"`
	Proto      string     `json:"proto"`
	Host       string     `json:"host"`
	URI        string     `json:"uri"`
	RemoteAddr string     `json:"remote_addr"`
	ClientIP   string     `json:"client_ip"`
	Scheme     string     `json:"scheme"`
	TLSInfo    string     `json:"tls"`
	ViaProxy   bool       `json:"via_proxy"`
	Headers    []headerKV `json:"headers"`
}

type kubeInfo struct {
	Available  bool   `json:"available"`
	Error      string `json:"error,omitempty"`
	APIServer  string `json:"api_server,omitempty"`
	GitVersion string `json:"git_version,omitempty"`
	Major      string `json:"major,omitempty"`
	Minor      string `json:"minor,omitempty"`
	Platform   string `json:"platform,omitempty"`
	BuildDate  string `json:"build_date,omitempty"`
	GoVersion  string `json:"go_version,omitempty"`
}

type plusInfo struct {
	Enabled bool        `json:"enabled"`
	Error   string      `json:"error,omitempty"`
	APIBase string      `json:"api_base,omitempty"`
	Nginx   nginxStatus `json:"nginx"`
	Caches  []cacheZone `json:"caches"`
}

type promInfo struct {
	Enabled    bool        `json:"enabled"`
	Error      string      `json:"error,omitempty"`
	APIBase    string      `json:"api_base,omitempty"`
	Controller []KV        `json:"controller"`
	Caches     []cacheZone `json:"caches"`
	Traffic    []KV        `json:"traffic"`
}

type nginxStatus struct {
	Version       string `json:"version"`
	Build         string `json:"build"`
	Address       string `json:"address"`
	Generation    int    `json:"generation"`
	LoadTimestamp string `json:"load_timestamp"`
	Timestamp     string `json:"timestamp"`
	Pid           int    `json:"pid"`
	Ppid          int    `json:"ppid"`
}

type cacheZone struct {
	Name    string `json:"name"`
	Size    string `json:"size"`
	MaxSize string `json:"max_size"`
	Cold    bool   `json:"cold"`
}

// ---- collectors ----

func collect(r *http.Request) pageData {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	d := pageData{
		Server: serverInfo{
			Version:  appVersion,
			Build:    buildTime,
			Go:       runtime.Version(),
			Uptime:   time.Since(started).Round(time.Second).String(),
			Hostname: host,
		},
		Request:   collectRequest(r),
		Pod:       collectPod(),
		Facts:     collectFacts(),
		Kube:      collectKube(ctx),
		Plus:      collectPlus(ctx),
		Prom:      collectProm(ctx),
		Generated: time.Now().Format("2006-01-02 15:04:05 MST"),
	}
	if demoMode() {
		applyDemo(&d)
	}
	return d
}

func collectRequest(r *http.Request) requestInfo {
	ri := requestInfo{
		Method:     r.Method,
		Proto:      r.Proto,
		Host:       r.Host,
		URI:        r.RequestURI,
		RemoteAddr: r.RemoteAddr,
		Scheme:     "http",
	}
	names := make([]string, 0, len(r.Header))
	for n := range r.Header {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fwd := strings.HasPrefix(n, "X-Forwarded") || n == "X-Real-Ip" || n == "Forwarded" || n == "Via"
		if fwd {
			ri.ViaProxy = true
		}
		ri.Headers = append(ri.Headers, headerKV{Name: n, Value: strings.Join(r.Header[n], ", "), Forwarded: fwd})
	}
	if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
		ri.Scheme = xfp
	} else if r.TLS != nil {
		ri.Scheme = "https"
	}
	switch {
	case r.Header.Get("X-Forwarded-For") != "":
		ri.ClientIP = strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	case r.Header.Get("X-Real-Ip") != "":
		ri.ClientIP = r.Header.Get("X-Real-Ip")
	default:
		ri.ClientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	switch {
	case r.TLS != nil:
		ri.TLSInfo = fmt.Sprintf("%s · %s (terminated at this pod)", tlsVersion(r.TLS.Version), tls.CipherSuiteName(r.TLS.CipherSuite))
	case ri.Scheme == "https":
		ri.TLSInfo = "terminated upstream (X-Forwarded-Proto: https)"
	default:
		ri.TLSInfo = "none"
	}
	return ri
}

func collectPod() []KV {
	order := []struct{ env, label string }{
		{"POD_NAME", "Pod"},
		{"POD_NAMESPACE", "Namespace"},
		{"POD_IP", "Pod IP"},
		{"NODE_NAME", "Node"},
		{"HOST_IP", "Node IP"},
		{"POD_SERVICE_ACCOUNT", "Service account"},
	}
	var out []KV
	for _, o := range order {
		if v := os.Getenv(o.env); v != "" {
			out = append(out, KV{o.label, v})
		}
	}
	if h, err := os.Hostname(); err == nil {
		out = append(out, KV{"Hostname", h})
	}
	out = append(out, KV{"Architecture", runtime.GOOS + "/" + runtime.GOARCH})
	return out
}

// collectFacts surfaces operator-supplied facts from env vars named PROBE_FACT_*.
// e.g. PROBE_FACT_NIM_Version=2.18 → shows "NIM Version: 2.18". Handy for NGINX
// Instance Manager / controller versions that aren't in the data-plane API.
func collectFacts() []KV {
	var out []KV
	for _, e := range os.Environ() {
		raw, ok := strings.CutPrefix(e, "PROBE_FACT_")
		if !ok {
			continue
		}
		if k, v, ok := strings.Cut(raw, "="); ok && v != "" {
			out = append(out, KV{strings.ReplaceAll(k, "_", " "), v})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var (
	kubeMu    sync.Mutex
	kubeCache *kubeInfo // the cluster version never changes during a pod's life
)

func collectKube(ctx context.Context) kubeInfo {
	kubeMu.Lock()
	defer kubeMu.Unlock()
	if kubeCache != nil && kubeCache.Available {
		return *kubeCache
	}
	ki := queryKube(ctx)
	kubeCache = &ki
	return ki
}

func queryKube(ctx context.Context) kubeInfo {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return kubeInfo{Error: "not running in a Kubernetes cluster (KUBERNETES_SERVICE_HOST unset)"}
	}
	api := "https://" + net.JoinHostPort(host, envDefault("KUBERNETES_SERVICE_PORT", "443"))
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return kubeInfo{APIServer: api, Error: "no service-account token: " + err.Error()}
	}
	caPEM, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return kubeInfo{APIServer: api, Error: "no cluster CA: " + err.Error()}
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	var v struct {
		Major, Minor, GitVersion, BuildDate, Platform, GoVersion string
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api+"/version", nil)
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if err := doJSON(client, req, &v); err != nil {
		return kubeInfo{APIServer: api, Error: err.Error()}
	}
	return kubeInfo{
		Available: true, APIServer: api,
		Major: v.Major, Minor: v.Minor, GitVersion: v.GitVersion,
		Platform: v.Platform, BuildDate: v.BuildDate, GoVersion: v.GoVersion,
	}
}

func collectPlus(ctx context.Context) plusInfo {
	base := strings.TrimRight(os.Getenv("NGINX_PLUS_API_URL"), "/")
	if base == "" {
		return plusInfo{Enabled: false}
	}
	out := plusInfo{Enabled: true, APIBase: base}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: os.Getenv("NGINX_PLUS_API_INSECURE") == "true", //nolint:gosec // opt-in for self-signed test endpoints
		MinVersion:         tls.VersionTLS12,
	}}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/nginx", nil)
	if err := doJSON(client, req, &out.Nginx); err != nil {
		out.Error = err.Error()
		return out
	}
	var caches map[string]struct {
		Size    int64 `json:"size"`
		MaxSize int64 `json:"max_size"`
		Cold    bool  `json:"cold"`
	}
	creq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/http/caches", nil)
	if err := doJSON(client, creq, &caches); err == nil {
		names := make([]string, 0, len(caches))
		for n := range caches {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			out.Caches = append(out.Caches, cacheZone{Name: n, Size: humanBytes(caches[n].Size), MaxSize: maxSize(caches[n].MaxSize), Cold: caches[n].Cold})
		}
	}
	return out
}

// collectProm pulls the controller's live metrics from Prometheus (no RBAC — the probe
// is just a read-only HTTP consumer; Prometheus already did the scraping). Each row
// renders only if its query returns data, so it degrades gracefully per-metric.
func collectProm(ctx context.Context) promInfo {
	base := strings.TrimRight(os.Getenv("PROMETHEUS_URL"), "/")
	if base == "" {
		return promInfo{Enabled: false}
	}
	out := promInfo{Enabled: true, APIBase: base}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: os.Getenv("PROMETHEUS_INSECURE") == "true", //nolint:gosec // opt-in for self-signed Prometheus
		MinVersion:         tls.VersionTLS12,
	}}}
	token := os.Getenv("PROMETHEUS_TOKEN")
	q := func(query string) []promSample {
		rs, _ := promQuery(ctx, client, base, token, query)
		return rs
	}

	// Build info doubles as the connectivity check — its error (not its emptiness) is surfaced.
	rs, err := promQuery(ctx, client, base, token, "nginx_ingress_controller_build_info")
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if len(rs) > 0 {
		m := rs[0].Metric
		if v := m["version"]; v != "" {
			out.Controller = append(out.Controller, KV{"Controller version", v})
		}
		if g := m["git_commit"]; g != "" {
			if len(g) > 12 {
				g = g[:12]
			}
			out.Controller = append(out.Controller, KV{"Git commit", g})
		}
	}
	if rs := q("nginx_ingress_controller_nginx_last_reload_status"); len(rs) > 0 {
		status := "failing"
		if rs[0].Value == "1" {
			status = "OK"
		}
		out.Controller = append(out.Controller, KV{"Last config reload", status})
	}
	if rs := q("sum(nginx_ingress_controller_nginx_reloads_total)"); len(rs) > 0 {
		out.Controller = append(out.Controller, KV{"Reloads", promInt(rs[0].Value)})
	}

	sizes, maxes := map[string]int64{}, map[string]int64{}
	for _, r := range q("nginxplus_cache_size") {
		sizes[r.Metric["cache"]] = promBytes(r.Value)
	}
	for _, r := range q("nginxplus_cache_max_size") {
		maxes[r.Metric["cache"]] = promBytes(r.Value)
	}
	zones := map[string]bool{}
	for n := range sizes {
		zones[n] = true
	}
	for n := range maxes {
		zones[n] = true
	}
	names := make([]string, 0, len(zones))
	for n := range zones {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Caches = append(out.Caches, cacheZone{Name: n, Size: humanBytes(sizes[n]), MaxSize: maxSize(maxes[n])})
	}

	if rs := q("sum(rate(nginxplus_http_requests_total[5m]))"); len(rs) > 0 {
		out.Traffic = append(out.Traffic, KV{"Requests/sec", promFloat(rs[0].Value)})
	}
	if rs := q("sum(nginxplus_connections_active)"); len(rs) > 0 {
		out.Traffic = append(out.Traffic, KV{"Active connections", promInt(rs[0].Value)})
	}
	return out
}

type promSample struct {
	Metric map[string]string
	Value  string
}

func promQuery(ctx context.Context, c *http.Client, base, token, query string) ([]promSample, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/query?query="+url.QueryEscape(query), nil)
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s", resp.Status)
	}
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("Prometheus query failed: %s", body.Error)
	}
	out := make([]promSample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s := promSample{Metric: r.Metric}
		if len(r.Value) == 2 {
			_ = json.Unmarshal(r.Value[1], &s.Value)
		}
		out = append(out, s)
	}
	return out, nil
}

func demoMode() bool {
	switch strings.ToLower(os.Getenv("PROBE_DEMO")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// applyDemo fills sample values for local previews and screenshots when there is no
// cluster. It is clearly flagged in the UI so it is never mistaken for live data.
func applyDemo(d *pageData) {
	d.Demo = true
	if !d.Kube.Available {
		d.Kube = kubeInfo{
			Available: true, APIServer: "https://10.96.0.1:443",
			Major: "1", Minor: "31", GitVersion: "v1.31.4",
			Platform: "linux/amd64", BuildDate: "2025-12-11T20:00:00Z", GoVersion: "go1.23.4",
		}
	}
	if !d.Plus.Enabled || d.Plus.Error != "" {
		d.Plus = plusInfo{
			Enabled: true, APIBase: "http://nginx-ingress.nginx-ingress/api (demo)",
			Nginx: nginxStatus{
				Version: "1.27.4 (nginx-plus-r34)", Build: "nginx-plus-r34", Address: "10.0.1.5",
				Generation: 3, LoadTimestamp: "2026-06-22T03:10:00Z", Pid: 21, Ppid: 1,
			},
			Caches: []cacheZone{
				{Name: "static_cache", Size: "128.0 MiB", MaxSize: "512.0 MiB", Cold: false},
				{Name: "api_cache", Size: "4.0 MiB", MaxSize: "256.0 MiB", Cold: true},
			},
		}
	}
	if !d.Prom.Enabled || d.Prom.Error != "" {
		d.Prom = promInfo{
			Enabled: true, APIBase: "http://prometheus-operated.monitoring.svc:9090 (demo)",
			Controller: []KV{
				{"Controller version", "v4.0.1"},
				{"Git commit", "a1b2c3d4e5f6"},
				{"Last config reload", "OK"},
				{"Reloads", "7"},
			},
			Caches: []cacheZone{
				{Name: "static_cache", Size: "128.0 MiB", MaxSize: "512.0 MiB"},
			},
			Traffic: []KV{
				{"Requests/sec", "42.17"},
				{"Active connections", "18"},
			},
		}
	}
}

// ---- helpers ----

func doJSON(c *http.Client, req *http.Request, v any) error {
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", req.URL, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func maxSize(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return humanBytes(n)
}

// Prometheus values arrive as strings holding a float; these coerce them for display.
func promBytes(s string) int64 { f, _ := strconv.ParseFloat(s, 64); return int64(f) }
func promInt(s string) string {
	f, _ := strconv.ParseFloat(s, 64)
	return strconv.FormatInt(int64(f), 10)
}

func promFloat(s string) string {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func tlsVersion(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS 1.3"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS10:
		return "TLS 1.0"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s ua=%q xff=%q", r.Method, r.URL.Path, r.Proto, r.UserAgent(), r.Header.Get("X-Forwarded-For"))
	})
}

func main() {
	addr := ":" + envDefault("PORT", "8080")
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(collect(r))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "index.html", collect(r)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	srv := &http.Server{Addr: addr, Handler: logMiddleware(mux), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("nginx-ingress-probe %s (built %s) listening on %s", appVersion, buildTime, addr)
	log.Fatal(srv.ListenAndServe())
}
