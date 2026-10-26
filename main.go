package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed template.yaml
var defaultTemplate []byte

//go:embed index.html
var indexHTML []byte

//go:embed hf.co.png
var hfIcon []byte

//go:embed huggingface_logo-noborder.svg
var hfLogoSVG []byte

var client = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second,
			// 禁止访问内网
			Control: func(network, address string, _ syscall.RawConn) error {
				host, _, _ := net.SplitHostPort(address)
				ip := net.ParseIP(host)
				if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
					return errors.New("禁止访问内网地址")
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

var (
	cacheDir   = "cache"
	cacheTTL   = 12 * time.Hour
	cacheLocks sync.Map
)

const maxSubs = 8

// token -> 订阅地址（多行=多源），落盘到 json
type store struct {
	mu     sync.Mutex
	path   string
	tokens map[string]string
}

func loadStore(path string) *store {
	s := &store{path: path, tokens: map[string]string{}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.tokens); err != nil {
			log.Fatalf("load %s: %v", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("load %s: %v", path, err)
	}
	return s
}

func (s *store) get(token string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.tokens[token]
	if !ok {
		return nil, false
	}
	return splitSubURLs(raw), true
}

// 获取已有 token；reset 时换新 token，旧的失效
func (s *store) issue(subKey string, reset bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := ""
	for t, u := range s.tokens {
		if u == subKey {
			old = t
			break
		}
	}
	if old != "" && !reset {
		return old, nil
	}
	if old == "" && reset {
		return "", errors.New("该订阅还没有生成过地址")
	}

	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)

	next := make(map[string]string, len(s.tokens)+1)
	for t, u := range s.tokens {
		if t != old {
			next[t] = u
		}
	}
	next[token] = subKey
	data, _ := json.MarshalIndent(next, "", "  ")
	if err := os.WriteFile(s.path, data, 0600); err != nil {
		return "", err
	}
	s.tokens = next
	return token, nil
}

func splitSubURLs(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func parseSubURLs(raw string) ([]string, string, error) {
	seen := map[string]bool{}
	var urls []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, "", fmt.Errorf("订阅地址无效: %s", line)
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		urls = append(urls, line)
	}
	if len(urls) == 0 {
		return nil, "", errors.New("请至少填写一个订阅地址")
	}
	if len(urls) > maxSubs {
		return nil, "", fmt.Errorf("最多 %d 个订阅源", maxSubs)
	}
	return urls, strings.Join(urls, "\n"), nil
}

func main() {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":5090"
	}
	dataFile := os.Getenv("DATA_FILE")
	if dataFile == "" {
		dataFile = "subs.json"
	}
	var allowHosts []string
	if v := os.Getenv("ALLOW_HOSTS"); v != "" {
		allowHosts = strings.Split(v, ",")
	}
	if v := os.Getenv("CACHE_DIR"); v != "" {
		cacheDir = v
	}
	if v := os.Getenv("RULE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatalf("RULE_TTL: %v", err)
		}
		cacheTTL = d
	}
	rateLimit := 100
	if v := os.Getenv("RATE_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			log.Fatalf("RATE_LIMIT: want positive integer")
		}
		rateLimit = n
	}
	subs := loadStore(dataFile)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})

	issue := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		reply := func(code int, v map[string]string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}

		var req struct {
			SubURL string `json:"sub_url"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
			return
		}
		urls, subKey, err := parseSubURLs(req.SubURL)
		if err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		for _, subURL := range urls {
			u, _ := url.Parse(subURL)
			if generatedLink(u, r, subs) {
				reply(http.StatusBadRequest, map[string]string{"error": "不能使用已生成的地址，请填写机场原始订阅"})
				return
			}
			if len(allowHosts) > 0 && !hostAllowed(u.Hostname(), allowHosts) {
				reply(http.StatusForbidden, map[string]string{"error": "不支持该订阅域名: " + u.Hostname()})
				return
			}
		}
		if raw, _, _, err := combineSubs(urls); err != nil {
			log.Printf("combine: %v", err)
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		} else if _, err := mergeRules(raw, "", "zh", ""); err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		token, err := subs.issue(subKey, r.URL.Path == "/api/reset")
		if err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		reply(http.StatusOK, map[string]string{"token": token})
	}
	http.HandleFunc("/api/token", issue)
	http.HandleFunc("/api/reset", issue)

	http.HandleFunc("/clash", func(w http.ResponseWriter, r *http.Request) {
		urls, ok := subs.get(r.URL.Query().Get("token"))
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		raw, userinfo, profile, err := combineSubs(urls)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		localBase := ""
		if r.URL.Query().Get("rules") == "local" {
			localBase = publicBase(r)
		}
		lang := r.URL.Query().Get("lang")
		if lang != "zh" {
			lang = "en"
		}
		out, err := mergeRules(raw, localBase, lang, publicBase(r))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if userinfo != "" {
			w.Header().Set("Subscription-Userinfo", userinfo)
		}
		w.Header().Set("Profile-Update-Interval", "24")
		w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(profile)+".yaml")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(out)
	})

	http.HandleFunc("/rules/", func(w http.ResponseWriter, r *http.Request) {
		data, err := getRule(strings.TrimPrefix(r.URL.Path, "/rules/"))
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	})

	log.Printf("listen on %s, rate %d/s", addr, rateLimit)
	http.HandleFunc("/icons/hf.co.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(hfIcon)
	})
	http.HandleFunc("/icons/huggingface_logo-noborder.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(hfLogoSVG)
	})
	log.Fatal(http.ListenAndServe(addr, newLimiter(rateLimit).wrap(http.DefaultServeMux)))
}

// 令牌桶：每秒最多 rate 个请求，桶容量等于 rate，超出直接拒绝
type limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newLimiter(n int) *limiter {
	return &limiter{rate: float64(n), tokens: float64(n), last: time.Now()}
}

func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.rate {
		l.tokens = l.rate
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func (l *limiter) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow() {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// JMS 订阅不带 Subscription-Userinfo，用流量接口补上
func jmsUsage(subURL string) string {
	u, err := url.Parse(subURL)
	if err != nil || !strings.HasSuffix(u.Path, "/getsub.php") {
		return ""
	}
	if h := u.Hostname(); !strings.Contains(h, "jms") && !strings.Contains(h, "justmysocks") {
		return ""
	}
	q := u.Query()
	if q.Get("service") == "" || q.Get("id") == "" {
		return ""
	}
	host := os.Getenv("JMS_BW_HOST")
	if host == "" {
		host = "justmysocks6.net"
	}
	api := "https://" + host + "/members/getbwcounter.php?" +
		url.Values{"service": {q.Get("service")}, "id": {q.Get("id")}}.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("jms usage: %v", err)
		return ""
	}
	defer resp.Body.Close()

	var bw struct {
		Limit int64 `json:"monthly_bw_limit_b"`
		Used  int64 `json:"bw_counter_b"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&bw); err != nil || bw.Limit == 0 {
		return ""
	}
	// JMS 按 1000 进制，客户端按 1024 显示
	k := float64(1<<30) / 1e9
	return fmt.Sprintf("upload=0; download=%d; total=%d", int64(float64(bw.Used)*k), int64(float64(bw.Limit)*k))
}

// 本服务发出的 /clash 链接不能再拿来生成，否则一条订阅会套出无数 token
func generatedLink(u *url.URL, r *http.Request, subs *store) bool {
	if strings.TrimSuffix(u.Path, "/") != "/clash" {
		if raw := u.Query().Get("url"); raw != "" {
			if inner, err := url.Parse(raw); err == nil && generatedLink(inner, r, subs) {
				return true
			}
		}
		return false
	}
	if t := u.Query().Get("token"); t != "" {
		if _, ok := subs.get(t); ok {
			return true
		}
	}
	return sameHost(u.Host, r.Host) || sameHost(u.Host, publicHost())
}

func sameHost(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ah, _, err := net.SplitHostPort(a)
	if err != nil {
		ah = a
	}
	bh, _, err := net.SplitHostPort(b)
	if err != nil {
		bh = b
	}
	return strings.EqualFold(ah, bh)
}

func publicHost() string {
	v := os.Getenv("PUBLIC_URL")
	if v == "" {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	return u.Host
}

func publicBase(r *http.Request) string {
	if v := os.Getenv("PUBLIC_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func parseUserinfo(s string) map[string]int64 {
	out := map[string]int64{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		if err != nil {
			continue
		}
		out[strings.TrimSpace(kv[0])] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// total>0 且 upload+download>=total，或已过 expire
func trafficExhausted(info string) bool {
	m := parseUserinfo(info)
	if m == nil {
		return false
	}
	if exp := m["expire"]; exp > 0 && time.Now().Unix() >= exp {
		return true
	}
	total := m["total"]
	if total <= 0 {
		return false
	}
	return m["upload"]+m["download"] >= total
}

func sumUserinfo(parts []string) string {
	var up, down, total, expire int64
	hasExpire := false
	any := false
	for _, p := range parts {
		m := parseUserinfo(p)
		if m == nil {
			continue
		}
		any = true
		up += m["upload"]
		down += m["download"]
		total += m["total"]
		if e := m["expire"]; e > 0 {
			if !hasExpire || e < expire {
				expire = e
				hasExpire = true
			}
		}
	}
	if !any {
		return ""
	}
	s := fmt.Sprintf("upload=%d; download=%d; total=%d", up, down, total)
	if hasExpire {
		s += fmt.Sprintf("; expire=%d", expire)
	}
	return s
}

func subHost(subURL string) string {
	u, err := url.Parse(subURL)
	if err != nil || u.Hostname() == "" {
		return "Clash"
	}
	return u.Hostname()
}

// 拉取并合并多源；流量用尽或过期的源整份跳过；userinfo 只叠加仍可用的源
func combineSubs(urls []string) ([]byte, string, string, error) {
	var all []any
	usedNames := map[string]int{}
	var infos []string
	var skipped int
	profile := "Clash"

	for i, subURL := range urls {
		raw, hdr, err := fetch(subURL)
		if err != nil {
			return nil, "", "", fmt.Errorf("%s: %w", subHost(subURL), err)
		}
		info := hdr.Get("Subscription-Userinfo")
		if info == "" {
			info = jmsUsage(subURL)
		}
		if trafficExhausted(info) {
			log.Printf("skip exhausted sub %s", subHost(subURL))
			skipped++
			continue
		}

		var sub map[string]any
		if err := yaml.Unmarshal(raw, &sub); err != nil {
			return nil, "", "", fmt.Errorf("%s: 不是有效的 Clash YAML", subHost(subURL))
		}
		proxies, _ := sub["proxies"].([]any)
		if len(proxies) == 0 {
			return nil, "", "", fmt.Errorf("%s: 订阅里没有节点", subHost(subURL))
		}
		for _, p := range proxies {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			if name == "" {
				continue
			}
			if n := usedNames[name]; n > 0 {
				usedNames[name] = n + 1
				pm["name"] = fmt.Sprintf("%s #%d", name, n+1)
			} else {
				usedNames[name] = 1
			}
			all = append(all, pm)
		}
		if info != "" {
			infos = append(infos, info)
		}
		if i == 0 || profile == "Clash" {
			profile = subHost(subURL)
		}
	}

	if len(all) == 0 {
		if skipped > 0 {
			return nil, "", "", errors.New("所有订阅流量已用尽或已过期")
		}
		return nil, "", "", errors.New("合并后没有节点")
	}
	if len(urls) > 1 {
		profile = "merged"
	}
	raw, err := yaml.Marshal(map[string]any{"proxies": all})
	if err != nil {
		return nil, "", "", err
	}
	return raw, sumUserinfo(infos), profile, nil
}

// 只允许模板里的规则；缓存过期才回源，回源失败用旧缓存
func getRule(name string) ([]byte, error) {
	tpl, err := loadTemplate()
	if err != nil {
		return nil, err
	}
	providers, _ := tpl["rule-providers"].(map[string]any)
	p, _ := providers[name].(map[string]any)
	if p == nil {
		return nil, os.ErrNotExist
	}
	src, _ := p["url"].(string)
	if src == "" {
		return nil, os.ErrNotExist
	}

	l, _ := cacheLocks.LoadOrStore(name, &sync.Mutex{})
	mu := l.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	file := filepath.Join(cacheDir, name)
	if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < cacheTTL {
		return os.ReadFile(file)
	}

	data, _, err := fetch(src)
	if err != nil {
		if old, rerr := os.ReadFile(file); rerr == nil {
			log.Printf("refresh rule %s: %v, use stale cache", name, err)
			return old, nil
		}
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0755); err == nil {
		tmp := file + ".tmp"
		if err := os.WriteFile(tmp, data, 0644); err == nil {
			os.Rename(tmp, file)
		}
	}
	return data, nil
}

func hostAllowed(host string, allow []string) bool {
	for _, h := range allow {
		h = strings.TrimSpace(h)
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func fetch(target string) ([]byte, http.Header, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "clash.meta")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.Header.Get("Cf-Mitigated") != "" || resp.Header.Get("Cf-Ray") != "" && resp.StatusCode == http.StatusForbidden {
			return nil, nil, fmt.Errorf("上游返回 %d，被 Cloudflare 拦截", resp.StatusCode)
		}
		return nil, nil, fmt.Errorf("上游返回 %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, nil, err
	}
	return raw, resp.Header, nil
}

func loadTemplate() (map[string]any, error) {
	tpl := defaultTemplate
	if path := os.Getenv("TEMPLATE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		tpl = b
	}
	var m map[string]any
	if err := yaml.Unmarshal(tpl, &m); err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	return m, nil
}

// 用模板覆盖上游同名 key
func mergeRules(raw []byte, localBase, lang, assetBase string) ([]byte, error) {
	var sub map[string]any
	if err := yaml.Unmarshal(raw, &sub); err != nil {
		return nil, fmt.Errorf("订阅不是有效的 Clash YAML: %w", err)
	}
	if proxies, _ := sub["proxies"].([]any); len(proxies) == 0 {
		return nil, fmt.Errorf("订阅里没有节点，请确认是 Clash 格式的订阅")
	}
	rules, err := loadTemplate()
	if err != nil {
		return nil, err
	}

	var en map[string]string
	if lang == "en" {
		en = map[string]string{}
		if rawMap, ok := rules["x-en"].(map[string]any); ok {
			for k, v := range rawMap {
				if s, ok := v.(string); ok {
					en[k] = s
				}
			}
		}
	}
	for k, v := range rules {
		if !strings.HasPrefix(k, "x-") {
			sub[k] = v
		}
	}
	if len(en) > 0 {
		renameGroups(sub, en)
	}
	absIcons(sub, assetBase)
	if localBase != "" {
		providers, _ := sub["rule-providers"].(map[string]any)
		for name, p := range providers {
			if m, ok := p.(map[string]any); ok {
				m["url"] = localBase + "/rules/" + url.PathEscape(name)
			}
		}
	}
	return yaml.Marshal(sub)
}

func absIcons(sub map[string]any, base string) {
	if base == "" {
		return
	}
	groups, _ := sub["proxy-groups"].([]any)
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		icon, _ := m["icon"].(string)
		if strings.HasPrefix(icon, "/") {
			m["icon"] = base + icon
		}
	}
}

func renameGroups(sub map[string]any, names map[string]string) {
	swap := func(s string) string {
		if n, ok := names[s]; ok {
			return n
		}
		return s
	}
	groups, _ := sub["proxy-groups"].([]any)
	for _, g := range groups {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["name"].(string); ok {
			m["name"] = swap(s)
		}
		list, _ := m["proxies"].([]any)
		for i, p := range list {
			if s, ok := p.(string); ok {
				list[i] = swap(s)
			}
		}
	}
	rules, _ := sub["rules"].([]any)
	for i, r := range rules {
		s, ok := r.(string)
		if !ok {
			continue
		}
		parts := strings.Split(s, ",")
		if len(parts) < 2 {
			continue
		}
		idx := len(parts) - 1
		if parts[idx] == "no-resolve" {
			idx--
		}
		if idx < 1 {
			continue
		}
		parts[idx] = swap(parts[idx])
		rules[i] = strings.Join(parts, ",")
	}
}
