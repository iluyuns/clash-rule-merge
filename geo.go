package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
	"github.com/oschwald/maxminddb-golang"
)

const (
	geoProbeTimeout = 8 * time.Second
	geoProbeTotal   = 90 * time.Second
	geoWorkers      = 32
	geoOKTTL        = 7 * 24 * time.Hour
	geoFailTTL      = 6 * time.Hour
	geoDBTTL        = 30 * 24 * time.Hour
)

// GEO_DB_URL 可覆盖；库约 8MB，GeoLite2 城市级
var geoDBURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"

// 经节点取出口 IP，第一个失败用第二个
var geoEcho = []string{"https://api.ipify.org", "https://icanhazip.com"}

// 出口位置缓存：指纹（协议|服务器|端口）-> 探测结果
type geoEntry struct {
	Country string    `json:"c"`
	City    string    `json:"y"`
	Code    string    `json:"d"`
	OK      bool      `json:"ok"`
	At      time.Time `json:"at"`
}

type geoCacheT struct {
	once sync.Once
	mu   sync.Mutex
	m    map[string]geoEntry
}

var geoCache = &geoCacheT{}

func (g *geoCacheT) init() {
	g.once.Do(func() {
		g.m = map[string]geoEntry{}
		if b, err := os.ReadFile(filepath.Join(cacheDir, "geo.json")); err == nil {
			json.Unmarshal(b, &g.m)
		}
	})
}

func (g *geoCacheT) get(fp string) (geoEntry, bool) {
	g.init()
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.m[fp]
	if !ok {
		return geoEntry{}, false
	}
	ttl := geoOKTTL
	if !e.OK {
		ttl = geoFailTTL
	}
	if time.Since(e.At) > ttl {
		return geoEntry{}, false
	}
	return e, true
}

func (g *geoCacheT) put(fp string, e geoEntry) {
	g.mu.Lock()
	g.m[fp] = e
	g.mu.Unlock()
}

func (g *geoCacheT) save() {
	g.mu.Lock()
	defer g.mu.Unlock()
	data, _ := json.Marshal(g.m)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return
	}
	tmp := filepath.Join(cacheDir, "geo.json.tmp")
	if err := os.WriteFile(tmp, data, 0644); err == nil {
		os.Rename(tmp, filepath.Join(cacheDir, "geo.json"))
	}
}

func geoFingerprint(pm map[string]any) string {
	typ, _ := pm["type"].(string)
	server, _ := pm["server"].(string)
	sum := sha1.Sum([]byte(typ + "|" + server + "|" + fmt.Sprint(pm["port"])))
	return hex.EncodeToString(sum[:8])
}

var (
	geoDBOnce sync.Once
	geoDB     *maxminddb.Reader
	geoDBErr  error
)

func geoReader() (*maxminddb.Reader, error) {
	geoDBOnce.Do(func() {
		file, err := geoDBFile()
		if err != nil {
			geoDBErr = err
			return
		}
		geoDB, geoDBErr = maxminddb.Open(file)
	})
	return geoDB, geoDBErr
}

// 缓存 30 天，回源失败用旧文件
func geoDBFile() (string, error) {
	file := filepath.Join(cacheDir, "geoip.metadb")
	if st, err := os.Stat(file); err == nil && time.Since(st.ModTime()) < geoDBTTL {
		return file, nil
	}
	dl := &http.Client{Timeout: 5 * time.Minute, Transport: client.Transport}
	resp, err := dl.Get(geoDBURL)
	if err == nil && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		err = fmt.Errorf("geoip.metadb: 上游返回 %d", resp.StatusCode)
	}
	if err != nil {
		if _, serr := os.Stat(file); serr == nil {
			log.Printf("refresh geoip.metadb: %v, use stale cache", err)
			return file, nil
		}
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return "", err
	}
	os.Rename(tmp, file)
	return file, nil
}

func pickName(names map[string]string) string {
	if n := names["zh-CN"]; n != "" {
		return n
	}
	return names["en"]
}

func lookupGeo(db *maxminddb.Reader, ip net.IP) geoEntry {
	fail := geoEntry{At: time.Now()}
	var rec struct {
		Country struct {
			ISOCode string            `maxminddb:"iso_code"`
			Names   map[string]string `maxminddb:"names"`
		} `maxminddb:"country"`
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
		Registered struct {
			ISOCode string            `maxminddb:"iso_code"`
			Names   map[string]string `maxminddb:"names"`
		} `maxminddb:"registered_country"`
	}
	if err := db.Lookup(ip, &rec); err != nil {
		return fail
	}
	country, code := pickName(rec.Country.Names), rec.Country.ISOCode
	if country == "" {
		country, code = pickName(rec.Registered.Names), rec.Registered.ISOCode
	}
	if country == "" {
		return fail
	}
	return geoEntry{Country: country, City: pickName(rec.City.Names), Code: code, OK: true, At: time.Now()}
}

// 经节点取真实出口 IP，再查本地 GeoIP 库；失败返回 OK=false
func probeGeo(ctx context.Context, pm map[string]any) geoEntry {
	fail := geoEntry{At: time.Now()}
	server, _ := pm["server"].(string)
	if ip := net.ParseIP(server); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()) {
		return fail
	}
	px, err := adapter.ParseProxy(pm)
	if err != nil {
		return fail
	}
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, ps, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				p64, err := strconv.ParseUint(ps, 10, 16)
				if err != nil {
					return nil, err
				}
				md := &C.Metadata{NetWork: C.TCP, Host: host, DstPort: uint16(p64)}
				conn, err := px.DialContext(ctx, md)
				if err != nil {
					return nil, err
				}
				return conn, nil
			},
		},
	}
	defer hc.CloseIdleConnections()

	var exitIP net.IP
	for _, u := range geoEcho {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil {
			exitIP = ip
			break
		}
	}
	if exitIP == nil {
		return fail
	}
	db, err := geoReader()
	if err != nil {
		return fail
	}
	return lookupGeo(db, exitIP)
}

func flagEmoji(code string) string {
	if len(code) != 2 {
		return ""
	}
	code = strings.ToUpper(code)
	if code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string(rune(0x1F1E6)+rune(code[0]-'A')) + string(rune(0x1F1E6)+rune(code[1]-'A'))
}

// 实测出口，按国家分组、节点改名「城市 01」；失败的保留原名进「其他」。返回改名后的 noAuto
func geoRename(sub map[string]any, noAuto []string) []string {
	proxies, _ := sub["proxies"].([]any)
	if len(proxies) == 0 {
		return noAuto
	}
	type item struct {
		pm   map[string]any
		name string
		fp   string
	}
	var items []item
	miss := map[string]map[string]any{}
	for _, p := range proxies {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := pm["name"].(string)
		if name == "" {
			continue
		}
		fp := geoFingerprint(pm)
		items = append(items, item{pm, name, fp})
		if _, ok := geoCache.get(fp); !ok {
			miss[fp] = pm
		}
	}

	if len(miss) > 0 {
		if _, err := geoReader(); err != nil {
			log.Printf("geo db: %v", err)
			miss = nil
		}
	}
	if len(miss) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), geoProbeTotal)
		defer cancel()
		sem := make(chan struct{}, geoWorkers)
		var wg sync.WaitGroup
		for fp, pm := range miss {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				pctx, pcancel := context.WithTimeout(ctx, geoProbeTimeout)
				e := probeGeo(pctx, pm)
				pcancel()
				if e.OK || ctx.Err() == nil {
					geoCache.put(fp, e)
				}
			}()
		}
		wg.Wait()
		geoCache.save()
	}

	type bucket struct {
		code  string
		nodes []string
	}
	buckets := map[string]*bucket{}
	var order []string
	seqs := map[string]int{}
	renamed := map[string]string{}
	var others []string
	for _, it := range items {
		e, ok := geoCache.get(it.fp)
		if !ok || !e.OK {
			others = append(others, it.name)
			continue
		}
		base := e.City
		if base == "" {
			base = e.Country
		}
		key := e.Country + "|" + base
		seqs[key]++
		name := fmt.Sprintf("%s %02d", base, seqs[key])
		it.pm["name"] = name
		renamed[it.name] = name
		b, ok := buckets[e.Country]
		if !ok {
			b = &bucket{code: e.Code}
			buckets[e.Country] = b
			order = append(order, e.Country)
		}
		b.nodes = append(b.nodes, name)
	}
	sort.SliceStable(order, func(i, j int) bool {
		return len(buckets[order[i]].nodes) > len(buckets[order[j]].nodes)
	})

	var names []any
	var groups []any
	addGroup := func(gname string, nodes []string) {
		list := make([]any, len(nodes))
		for i, n := range nodes {
			list[i] = n
		}
		groups = append(groups, map[string]any{"name": gname, "type": "select", "proxies": list})
		names = append(names, gname)
	}
	for _, country := range order {
		b := buckets[country]
		gname := country
		if f := flagEmoji(b.code); f != "" {
			gname = f + " " + country
		}
		addGroup(gname, b.nodes)
	}
	if len(others) > 0 {
		addGroup("其他", others)
	}

	gs, _ := sub["proxy-groups"].([]any)
	for _, g := range gs {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n == "节点选择" {
			delete(m, "include-all")
			m["proxies"] = append([]any{"自动选择", "直连"}, names...)
		}
	}
	sub["proxy-groups"] = append(gs, groups...)

	for i, n := range noAuto {
		if r, ok := renamed[n]; ok {
			noAuto[i] = r
		}
	}
	return noAuto
}
