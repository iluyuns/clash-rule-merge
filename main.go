package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed template.yaml
var defaultTemplate []byte

//go:embed index.html
var indexHTML []byte

var client = &http.Client{Timeout: 20 * time.Second}

var (
	cacheDir   = "cache"
	cacheTTL   = 12 * time.Hour
	cacheLocks sync.Map
)

// token -> sub_url，落盘到 json
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

func (s *store) get(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.tokens[token]
	return u, ok
}

// 获取已有 token；reset 时换新 token，旧的失效
func (s *store) issue(subURL string, reset bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old := ""
	for t, u := range s.tokens {
		if u == subURL {
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
	next[token] = subURL
	data, _ := json.MarshalIndent(next, "", "  ")
	if err := os.WriteFile(s.path, data, 0600); err != nil {
		return "", err
	}
	s.tokens = next
	return token, nil
}

func main() {
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
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
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
			return
		}
		subURL := strings.TrimSpace(req.SubURL)
		u, err := url.Parse(subURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			reply(http.StatusBadRequest, map[string]string{"error": "订阅地址无效"})
			return
		}
		if len(allowHosts) > 0 && !hostAllowed(u.Hostname(), allowHosts) {
			reply(http.StatusForbidden, map[string]string{"error": "不支持该订阅域名"})
			return
		}
		if raw, _, err := fetch(subURL); err != nil {
			reply(http.StatusBadGateway, map[string]string{"error": "拉取订阅失败: " + err.Error()})
			return
		} else if _, err := mergeRules(raw, ""); err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		token, err := subs.issue(subURL, r.URL.Path == "/api/reset")
		if err != nil {
			reply(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		reply(http.StatusOK, map[string]string{"token": token})
	}
	http.HandleFunc("/api/token", issue)
	http.HandleFunc("/api/reset", issue)

	http.HandleFunc("/clash", func(w http.ResponseWriter, r *http.Request) {
		subURL, ok := subs.get(r.URL.Query().Get("token"))
		if !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		raw, header, err := fetch(subURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		localBase := ""
		if r.URL.Query().Get("rules") == "local" {
			localBase = publicBase(r)
		}
		out, err := mergeRules(raw, localBase)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if v := header.Get("Subscription-Userinfo"); v != "" {
			w.Header().Set("Subscription-Userinfo", v)
		}
		name := "Clash"
		if u, err := url.Parse(subURL); err == nil {
			name = u.Hostname()
		}
		w.Header().Set("Profile-Update-Interval", "24")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
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

	log.Printf("listen on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
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

// 只允许模板里的规则；缓存过期才回源，回源失败用旧缓存
func getRule(name string) ([]byte, error) {
	tpl, err := loadTemplate()
	if err != nil {
		return nil, err
	}
	providers, _ := tpl["rule-providers"].(map[string]any)
	p, _ := providers[name].(map[string]any)
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
	resp, err := client.Get(target)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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

// 用模板覆盖上游同名 key；localBase 非空时规则改走本服务器
func mergeRules(raw []byte, localBase string) ([]byte, error) {
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

	for k, v := range rules {
		if !strings.HasPrefix(k, "x-") {
			sub[k] = v
		}
	}
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
