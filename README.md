# clash-rule-merge

给 Clash（YAML）订阅自动集成分流规则的小服务：节点原样保留，只追加分组和规则。

```
机场 Clash 订阅 ──► clash-rule-merge ──► 带分流规则的新订阅 ──► Clash 客户端
                     (覆盖 proxy-groups / rule-providers / rules)
```

## 为什么做这个

很多机场（例如 JMS）已经直接提供 Clash YAML 格式的订阅，但只有节点，规则只有一条 `MATCH`，所有流量都走代理。常见做法是套一层订阅转换（subconverter），但有几个问题：

- **不需要转换**：订阅本来就是 Clash 格式，只缺规则，没必要再走一遍完整的格式转换。
- **协议支持**：原版 subconverter 不支持 VLESS / Reality，转换后这类节点会被丢掉。
- **隐私**：使用公共转换后端，相当于把含密码的订阅地址交给第三方。

所以这里换了思路：自己部署、不做转换，只把规则“合并”进原订阅；规则本身引用社区每日更新的规则集，由客户端自动下载更新。

## 工作原理

1. 用户在网页输入原订阅地址，服务端校验能正常拉取、解析后，生成一个随机 token。
2. 客户端订阅 `/clash?token=xxx` 时，服务端实时拉取原订阅，用 `template.yaml` 覆盖其中的 `proxy-groups`、`rule-providers`、`rules`，其余字段（节点、端口等）保持不变。
3. 分组使用 mihomo 的 `include-all-proxies` 自动包含全部节点，节点增减无需改配置。
4. 规则集以 `rule-providers` 形式引用，客户端每 24 小时自动更新。
5. 可选：链接加上 `&rules=local` 后，规则集改从本服务器下载，服务器回源并缓存（默认 12 小时）。用于 jsDelivr 在部分地区或网络下不稳定的情况。

## 使用的项目

| 项目 | 用途 |
|---|---|
| [MetaCubeX/meta-rules-dat](https://github.com/MetaCubeX/meta-rules-dat) | 规则来源。mihomo 官方规则数据，每日自动构建，使用 `meta` 分支的 `mrs` 二进制规则集（体积小、加载快） |
| [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community)、[Loyalsoldier/v2ray-rules-dat](https://github.com/Loyalsoldier/v2ray-rules-dat)、[blackmatrix7/ios_rule_script](https://github.com/blackmatrix7/ios_rule_script) | meta-rules-dat 的上游数据源（geosite / geoip） |
| [jsDelivr](https://www.jsdelivr.com) | GitHub 仓库的 CDN 镜像，默认规则下载地址 |
| [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo) | 目标客户端内核，支持 `mrs` 规则集、`include-all-proxies`、VLESS Reality 等 |
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
  -p 8080:8080 \
  -e ALLOW_HOSTS=jmssub.net \
  -v clash-rule-merge-data:/data \
  iluyuns/clash-rule-merge:latest
```

更新：`docker compose pull && docker compose up -d`

### 源码运行

```bash
go run .
```

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `LISTEN` | `:8080` | 监听地址 |
| `ALLOW_HOSTS` | 空（不限制） | 允许的订阅域名，逗号分隔，含子域名。**公网部署强烈建议设置** |
| `DATA_FILE` | `subs.json`（镜像内 `/data/subs.json`） | token 与订阅地址的映射 |
| `CACHE_DIR` | `cache`（镜像内 `/data/cache`） | 规则缓存目录 |
| `RULE_TTL` | `12h` | 规则缓存有效期 |
| `PUBLIC_URL` | 自动识别 | 对外访问地址，例如 `https://sub.example.com`，反向代理后建议设置 |
| `TEMPLATE` | 内置 `template.yaml` | 外部模板路径，每次请求重新读取，修改无需重启 |

## 使用

1. 打开 `http://服务器:8080/`。
2. 粘贴机场的 Clash 格式订阅地址，点「生成地址」。
3. 复制新地址，在客户端中导入订阅。

- 同一订阅重复生成得到同一地址。
- 地址泄露时，输入原订阅地址点「重置地址」，旧地址立即失效。

### 客户端要求

需要 mihomo（Clash Meta）内核：Clash Verge Rev、FlClash、Clash Meta for Android、Stash 等。原版 Clash 已停更，不支持 `mrs` 规则集和 VLESS 等协议。

## 自定义规则

编辑 [`template.yaml`](template.yaml)：

- 新增规则集：在 meta-rules-dat 的 [`geo/geosite`](https://github.com/MetaCubeX/meta-rules-dat/tree/meta/geo/geosite) 或 [`geo/geoip`](https://github.com/MetaCubeX/meta-rules-dat/tree/meta/geo/geoip) 找到文件名，在 `rule-providers` 和 `rules` 各加一行，按需加分组。
- 更换镜像：批量替换 `testingcf.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@meta`。
- `x-` 开头的顶层 key 不会输出，可用于 YAML 锚点。

## 安全说明

- 页面无登录，持有原订阅地址即可获取或重置对应 token，请勿泄露原订阅地址。
- `subs.json` 保存了所有订阅地址，请妥善保管，不要提交到仓库。
- 建议通过 HTTPS 反向代理对外提供服务。

## 发布

推送到 `main` 分支或打 `v*` 标签后，GitHub Actions 自动构建 `linux/amd64`、`linux/arm64` 镜像并推送到 Docker Hub。需在仓库 Secrets 中配置 `DOCKERHUB_USERNAME`、`DOCKERHUB_TOKEN`。

## License

[MIT](LICENSE)
