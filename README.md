# clash-rule-merge

给 Clash（YAML）订阅自动集成分流规则的小服务：节点原样保留，只追加分组和规则。不做协议转换。

**部署案例**：<https://crm.minis.app/>（仅供体验，介意订阅地址经过第三方的建议[自行部署](#部署)）

```
机场 / 自建 Clash 订阅（可多个）
        │
        ▼
  clash-rule-merge  ──► 合并节点 + 覆盖分组/规则 ──► Clash / Stash / mihomo
```

## 适用场景

同一套规则，两种用法，在客户端里改「国内」分组即可：

| | 住在中国 | 住在国外 |
|---|---|---|
| Google / YouTube / ChatGPT | 走代理 | 直连或走代理均可 |
| 微信 / 支付宝 / 钉钉 / 国内站 | **直连**（默认） | 选一个 **中国出口** 节点，避免海外 IP 被风控 |
| 局域网 / 打印机 / 公司内网 | 始终直连 | 始终直连 |

「中国出口」通常是住宅宽带或国内 VPS，不是机场里延迟最低的那条。延迟低的机房 IP 打国内站，照样会被风控。

## 为什么做这个

很多机场（例如 JMS）已经直接提供 Clash YAML 格式的订阅，但只有节点，规则只有一条 `MATCH`，所有流量都走代理。常见做法是套一层订阅转换（subconverter），但有几个问题：

- **不需要转换**：订阅本来就是 Clash 格式，只缺规则，没必要再走一遍完整的格式转换。
- **协议支持**：原版 subconverter 不支持 VLESS / Reality，转换后这类节点会被丢掉。
- **隐私**：使用公共转换后端，相当于把含密码的订阅地址交给第三方。

所以这里换了思路：自己部署、不做转换，只把规则“合并”进原订阅；规则本身引用社区每日更新的规则集，由客户端自动下载更新。也支持一次粘贴多个订阅源（机场 + 自建等）合并成一条。

## AUTO 是什么

网页每个订阅源旁有一个 **AUTO** 勾选，默认打开。

- **勾选**：该源节点进入「自动选择」（url-test），按延迟自动切到最快的一条。日常翻墙用这个。
- **取消**：节点仍出现在「节点选择」和「国内」里，**不参与**延迟竞赛。适合住宅宽带、家庭网关这类出口：延迟往往不是最低，但 IP 像真人家里，给国内站用才有意义。若放进 AUTO，会被更快的机房节点抢走，国内站照样风控。

至少保留一个源勾选 AUTO，否则「自动选择」里没有节点。

## 工作原理

1. 用户在网页填写一个或多个 Clash YAML 订阅（每栏一个，最多 8 个），可按源取消 AUTO；服务端校验能拉取、解析后生成随机 token。
2. 客户端订阅 `/clash?token=xxx` 时，服务端实时拉取各源、合并 `proxies`（重名加 `#2`），再用 `template.yaml` 覆盖 `proxy-groups` / `rule-providers` / `rules`。未勾选 AUTO 的节点写入「自动选择」的 `exclude-filter`。
3. 某源 `Subscription-Userinfo` 显示流量用尽或已过期时，该源节点整份跳过；客户端流量头只叠加仍可用的源。
4. 分组使用 `include-all` 自动包含全部节点。Stash 与 mihomo 都支持；`include-all-proxies` 只有 mihomo 认。
5. 规则集以 `rule-providers` 形式引用，客户端约每 24 小时自动更新。可选 `&rules=local` 改从本服务器下载并缓存（默认 12 小时）。
6. 「国内」分组默认 `DIRECT`。住在国外或公司内网封国内站时，在客户端把「国内」改成你的中国出口节点，不要改模板去写死某个节点名。

## 使用的项目

| 项目 | 用途 |
|---|---|
| [MetaCubeX/meta-rules-dat](https://github.com/MetaCubeX/meta-rules-dat) | 规则来源。mihomo 官方规则数据，每日自动构建，使用 `meta` 分支的 `mrs` 二进制规则集（体积小、加载快） |
| [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community)、[Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat)、[blackmatrix7/ios_rule_script](https://github.com/blackmatrix7/ios_rule_script) | meta-rules-dat 的上游数据源（geosite / geoip） |
| [jsDelivr](https://www.jsdelivr.com) | GitHub 仓库的 CDN 镜像，默认规则下载地址 |
| [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) | 目标客户端内核，支持 `mrs` 规则集、`include-all`、VLESS Reality 等 |
| [go-yaml/yaml](https://github.com/go-yaml/yaml) | YAML 解析 |
| [ACL4SSR](https://github.com/ACL4SSR/ACL4SSR) / [zsokami/ACL4SSR](https://github.com/zsokami/ACL4SSR) | 分组设计参考 |

## 部署

### Docker Compose（推荐）

下载 [`docker-compose.yml`](docker-compose.yml)，按需修改环境变量后：

```bash
docker compose up -d
```

### Docker

```bash
docker run -d --name clash-rule-merge --restart unless-stopped \
  -p 5090:5090 \
  -v clash-rule-merge-data:/data \
  iuupub/clash-rule-merge:latest
```

更新：`docker compose pull && docker compose up -d`

### 绑定域名（HTTPS）

以 `sub.example.com` 为例：

1. **DNS**：添加 A 记录 `sub.example.com` → 服务器公网 IP。
2. **compose**：
   - 端口改为 `"127.0.0.1:5090:5090"`，只允许本机反向代理访问，不直接暴露 HTTP。
   - 设置 `PUBLIC_URL: https://sub.example.com`，保证“从本服务器下载规则”生成的地址正确。
   - `docker compose up -d` 使配置生效。
3. **反向代理**（二选一）：

   **Caddy**（自动申请、续期证书）`/etc/caddy/Caddyfile`：

   ```
   sub.example.com {
       reverse_proxy 127.0.0.1:5090
   }
   ```

   **Nginx**（证书可用 certbot 申请）：

   ```nginx
   server {
       listen 443 ssl;
       server_name sub.example.com;
       ssl_certificate     /etc/letsencrypt/live/sub.example.com/fullchain.pem;
       ssl_certificate_key /etc/letsencrypt/live/sub.example.com/privkey.pem;

       location / {
           proxy_pass http://127.0.0.1:5090;
           proxy_set_header Host $host;
           proxy_set_header X-Forwarded-Proto $scheme;
       }
   }
   ```

4. 打开 `https://sub.example.com/` 即可使用。防火墙需放行 80、443（Caddy / certbot 申请证书要用 80）。

### 源码运行

```bash
go run .
```

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `LISTEN` | `:5090` | 监听地址 |
| `RATE_LIMIT` | `100` | 全局限流，每秒最多接受的请求数，超出返回 429 |
| `ALLOW_HOSTS` | 空（不限制） | 允许的订阅域名，逗号分隔，含子域名。内网地址始终禁止访问 |
| `DATA_FILE` | `subs.json`（镜像内 `/data/subs.json`） | token 与订阅地址的映射 |
| `CACHE_DIR` | `cache`（镜像内 `/data/cache`） | 规则缓存目录 |
| `RULE_TTL` | `12h` | 规则缓存有效期 |
| `PUBLIC_URL` | 自动识别 | 对外访问地址，例如 `https://sub.example.com`，反向代理后建议设置 |
| `TEMPLATE` | 内置 `template.yaml` | 外部模板路径，每次请求重新读取，修改无需重启 |
| `JMS_BW_HOST` | `justmysocks6.net` | JMS 流量查询接口域名，用于在客户端显示已用/总流量 |

## 使用

1. 打开 `http://服务器:5090/`。
2. 每个订阅填一栏（最多 8 个）。机场源勾选 AUTO；住宅/中国出口源取消 AUTO。点「生成地址」。
3. 复制新地址，在客户端中导入订阅。
4. 住在国外时：打开「国内」分组，选那个中国出口节点。住在中国时保持 `DIRECT` 即可。

- 多源会合并节点；同名节点自动加 `#2` 后缀。
- 某源 `upload+download >= total` 或已过 `expire` 时，该源节点整份跳过；客户端流量只叠加仍可用的源。
- 同一组订阅（含 AUTO 勾选）重复生成得到同一地址。
- 地址泄露时，输入相同订阅列表点「重置地址」，旧地址立即失效。

### 客户端要求

需要 mihomo（Clash Meta）内核：Clash Verge Rev、FlClash、Clash Meta for Android、Stash 等。原版 Clash 已停更，不支持 `mrs` 规则集和 VLESS 等协议。

## 自定义规则

编辑 [`template.yaml`](template.yaml)：

- 新增规则集：在 meta-rules-dat 的 [`geo/geosite`](https://github.com/MetaCubeX/meta-rules-dat/tree/meta/geo/geosite) 或 [`geo/geoip`](https://github.com/MetaCubeX/meta-rules-dat/tree/meta/geo/geoip) 找到文件名，在 `rule-providers` 和 `rules` 各加一行，按需加分组。
- 更换镜像：批量替换 `testingcf.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta`。
- `x-` 开头的顶层 key 不会输出，可用于 YAML 锚点。
- 不要把某个用户的节点名（例如「家」）写进模板。`proxies` 里只放分组名和 `DIRECT` / `REJECT`；具体节点靠 `include-all` 由订阅填入。

## 安全说明

- 页面无登录，持有原订阅地址即可获取或重置对应 token，请勿泄露原订阅地址。
- `subs.json` 保存了所有订阅地址，请妥善保管，不要提交到仓库。
- 建议通过 HTTPS 反向代理对外提供服务。

<!-- 
发布
git push                    # 推送 dev 分支，不会触发构建
git tag v1.0.0
git push origin v1.0.0      # 触发构建，生成 1.0.0、1.0、latest 三个镜像标签 
-->

## License

[MIT](LICENSE)
