# pve-web

`pve-web` 是獨立於 `pve-console` TUI 的 Proxmox VE dashboard。Go backend 負責 Proxmox API、shared cache、背景 refresh 與 guest power task；React/Vite frontend 負責瀏覽器畫面。production 不需要常駐 Node.js。

## 目前範圍

- target/node/guest dashboard
- 固定排序：target、node、guest 及 storage refresh 後不跳動
- node CPU/memory 與 guest CPU/memory 的近五分鐘資料
- Proxmox API connection/authentication/permission/error 狀態
- guest start、shutdown、stop、reboot
- Node Shell（xterm.js + Proxmox termproxy，無 10 秒閒置斷線限制）
- Guest Console（VM/CT，noVNC + Proxmox vncproxy）
- UPID task progress 與 guest status 顯示
- optional HTTPS certificate generation 規劃介面
- FreeBSD amd64 優先，Linux amd64、Windows amd64 可 build
- 不提供 Web user login；請用 Nginx 內網來源限制保護
- 不提供 node power 或 package upgrade

## Path

所有 Web path 使用 `/pve-web/`，不使用泛用的 `/api/`：

```text
/pve-web/
/pve-web/data/overview
/pve-web/data/targets/{targetID}
/pve-web/operation/guests/{type}/{vmid}/power
/pve-web/tasks/{jobID}
/pve-web/health
/pve-web/version
```

## 設定

複製 `pve-web.yaml.example` 為 `pve-web.yaml`。預設 server 只 listen `127.0.0.1:8080`，由 Nginx 對外提供 `/pve-web/`。

預設允許的管理網段是：

```text
172.20.0.0/16
172.24.0.0/16
```

Go backend 本身不做使用者登入；Nginx 必須至少配置：

```nginx
location /pve-web/ {
    allow 172.20.0.0/16;
    allow 172.24.0.0/16;
    deny all;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_pass http://127.0.0.1:8080;
}
```

Node Shell 與 Guest Console 都透過 WebSocket 走同一個 `/pve-web/` location。上面
三行 proxy 設定是必要條件；缺少時 HTTP dashboard 仍可運作，但瀏覽器 WebSocket 會以
code `1006` 失敗，Console 開啟後很快消失。若後端改用 HTTPS，瀏覽器會自動使用
`wss://`，noVNC 的 secure-context 警告（僅出現在 Guest Console，因為它走
noVNC/RFB）也會隨之消失。

Node Shell 走 Proxmox 的 `termproxy`（xterm.js 用），**沒有** idle-shell 逾時；
Guest Console（VM/CT）走 `vncproxy`，直接連到 QEMU/LXC 自己的 VNC server，也沒有
額外的 idle 逾時。舊版 Node Shell（`vncshell`/`vncterm`）曾有 Proxmox 端硬編碼的
10 秒閒置斷線限制，已在改用 termproxy 後徹底解決，不需要任何前端假輸入或
keepalive 技巧。

If the backend log reports `websocket: the client is not using the websocket
protocol: 'upgrade' token not found in 'Connection' header`, the active Nginx
configuration is still stripping or overwriting the header. On the FreeBSD
host, inspect the effective configuration rather than only an unused file:

```sh
nginx -T 2>&1 | grep -E 'proxy_(http_version|set_header|pass)'
nginx -t && service nginx reload
```

Ensure there is no later `proxy_set_header Connection "";` in the matching
location. The `Upgrade` and `Connection` directives must be inside the active
`location /pve-web/` block, and Nginx must be reloaded after changing them.

## Credential migration

Windows DPAPI 不能在 FreeBSD/Linux 解密。現有 TUI 提供未公開於 help 的 migration command：

```powershell
pve-console.exe target export
```

它會在 `pve-console.exe` 所在目錄產生：

```text
pve-web-credential.json
```

檔案包含所有 target 的 endpoints、user、token name 與 token value。這是完整 Proxmox credential，必須透過安全方式搬移，匯入後刪除。

Web backend 使用：

```text
pve-web.yaml
pve-web-credentials.json
```

設定與 credential 分離，credential 檔案應設為 `600`。`pve-web-credential.json` 是交換檔，不是 runtime 檔案。

## Build

需要 Go 1.22、Node.js/npm。先 build frontend，再 build backend：

```sh
sh build.sh                 # default: FreeBSD amd64
sh build.sh freebsd
sh build.sh linux
sh build.sh windows
sh build.sh all
```

目前只產生 amd64。輸出在 `dist/{platform}-amd64/`，包含 binary、frontend archive、checksums。

版本可由環境變數指定：

```sh
PVE_WEB_VERSION=v0.1.0 sh build.sh freebsd
```

`build.sh` 也會產生可直接發布的 `dist/webroot/`。使用者不需要手動建立 `releases`、搬移檔案或調整權限：

```text
dist/webroot/
├── pve-web.yaml.example
├── pve_web.in
└── releases/
    ├── latest.json
    └── v0.1.0/
        └── freebsd-amd64/
            ├── pve-web
            ├── frontend.tar.gz
            └── checksums.txt
```

`build.sh` 預設也會將完整專案同步到：

```text
/mnt/pve-web/
```

所以 Alpine/Nginx 的 `/pve-web/` document root 可以直接指向：

```text
/mnt/pve-web/
```

`dist/webroot` 已設定目錄 `755`、一般檔案 `644`、binary `755`，可由 Alpine Web server 直接讀取。不要將 `pve-web.yaml`、`pve-web-credentials.json`、API token 或 private key 放入此 webroot。

## FreeBSD deploy

部署 script 可以單獨複製到 FreeBSD，從內網發布網站下載 binary/frontend，不需要先打包整個專案：

```sh
sh deploy.sh install
```

`install` 或 `upgrade` 完成後會檢查設定與 credential。若 credential 已存在，script 會設定 `pve_web_enable=YES`、啟動 FreeBSD service，並輸出本機 health check 與 log 指令；若 credential 尚未存在，script 會列出匯入 credential、確認 target、重新執行 install 的步驟。

Windows export 的 portable 檔名是 `pve-web-credential.json`。將它放到 `/usr/local/etc/pve-web/` 後重新執行 `sh deploy.sh install`，script 會轉成 service 使用的 `/usr/local/etc/pve-web/pve-web-credentials.json`，並設定檔案權限為 `600`。

未指定 URL 時預設使用 `http://172.20.1.6/pve-web`。發布網站需提供：

```text
/pve-web/releases/latest.json
/pve-web/releases/{version}/freebsd-amd64/pve-web
/pve-web/releases/{version}/freebsd-amd64/frontend.tar.gz
/pve-web/releases/{version}/freebsd-amd64/checksums.txt
```

可用操作：

```sh
sh deploy.sh install
sh deploy.sh upgrade
sh deploy.sh status
sh deploy.sh version
sh deploy.sh rollback
sh deploy.sh uninstall
```

upgrade 會比較 `latest.json` 與已安裝版本；版本相同就不重裝。升級會保留設定、credential、certificate、log 與 runtime data，並保存前一版 binary 供 rollback。部署網站是管理員手工維護的可信內網來源，因此 checksum 主要用來避免檔案傳輸不完整。

## Cross compile

```sh
GOOS=freebsd GOARCH=amd64 go build ./cmd/pve-web
GOOS=linux GOARCH=amd64 go build ./cmd/pve-web
GOOS=windows GOARCH=amd64 go build ./cmd/pve-web
```

## Nginx static/frontend options

Frontend 可以由 Nginx 提供，也可以由 Go server 提供。若使用 `deploy.sh`，frontend 預設放在：

```text
/usr/local/share/pve-web/frontend
```

最簡單的配置是讓 Nginx proxy 全部 `/pve-web/` 到 Go backend。若要由 Nginx 提供 static files，請確保 React base path 保持 `/pve-web/`，並將 `/pve-web/assets/` 指向 frontend assets；其餘 data/operation/tasks path proxy 到 backend。

`127.0.0.1:8080` 只可在 FreeBSD 主機本機使用，不是給使用者工作站開啟的 URL。如果 Nginx/release host 是 `172.20.1.6` 而 `pve-web` 在另一台 FreeBSD，FreeBSD 的 `server.listen` 必須改為可供 Nginx 連線的位址，例如 `0.0.0.0:8080`，Nginx 使用 FreeBSD 的實際內網 IP：

```nginx
location /pve-web/ {
    allow 172.20.0.0/16;
    allow 172.24.0.0/16;
    deny all;
    proxy_pass http://<freebsd-ip>:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
```

完成 proxy 後，使用者才應開啟 `http://172.20.1.6/pve-web/`。直接開啟 `http://127.0.0.1:8080/pve-web/` 只適用於在 FreeBSD 主機本機執行瀏覽器。

## HTTPS

HTTP 預設保持可用，HTTPS 預設關閉，不會因尚未產生 certificate 而無法啟動。正式 certificate management page 與 self-signed certificate generation 可在不影響 HTTP 的情況下啟用；產生 certificate 不會自動切換 HTTPS。Certificate 與 PKCS#8 private key 預設寫入 `/usr/local/etc/pve-web/pve-web.crt` 和 `/usr/local/etc/pve-web/pve-web.key`，產生時會另外保存非敏感的上次設定到 `/usr/local/etc/pve-web/pve-web-certificate.json`，可直接提供給 Nginx；重新產生會覆寫兩個檔案。效期可設定 1 到 3650 天，825 天以上適合內部或實驗室用途。Certificates 視窗可先 Load previous certificate，確認上次資料後才顯示 Regenerate，完成後提供 `.crt` 與 `.key` 下載按鈕。

## Guest power safety

操作確認會列出 target、node、guest type、VMID、guest name、目前狀態與即將執行的 action。running guest 可 shutdown/stop/reboot；stopped guest 可 start；其他狀態的 action 由 backend disable。task 完成後會再次 refresh guest data。

## 技術文件

更深入的技術參考文件放在 [`docs/`](./docs/)：

- [`docs/architecture.md`](./docs/architecture.md)：整體架構、執行期資料流、設定與 credential 分離的方式。
- [`docs/console-protocols.md`](./docs/console-protocols.md)：Node Shell 與 Guest Console 實際使用的 Proxmox API、底層協定差異，以及開發過程中踩過的坑與根因（idle timeout、API schema 差異、票證驗證端點、noVNC 鍵盤焦點）。
- [`docs/logging.md`](./docs/logging.md)：log 檔設定、會記錄哪些事件、常見問題怎麼從 log 判斷。
