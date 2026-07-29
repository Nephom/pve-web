# 架構總覽

給接手或回頭維護這個專案的人快速建立心理模型用的文件。細部的 console 協定請看
[`docs/console-protocols.md`](./console-protocols.md)，log 相關請看
[`docs/logging.md`](./logging.md)，部署/建置步驟請看根目錄的 `README.md`。

## 專案結構

```text
cmd/pve-web/          進入點：讀 config/credential、組出 http.Server、啟動背景 refresher
internal/config/      pve-web.yaml 的結構與驗證
internal/credentials/ pve-web-credentials.json（含 pve-web-credential.json 匯入相容）
internal/proxmox/     Proxmox VE REST/WebSocket client（唯一會直接呼叫 Proxmox API 的地方）
internal/runtime/     執行期共享狀態（config + credentials + 每個 target 的 *proxmox.Client）
internal/cache/       背景 refresh 寫入、HTTP handler 讀取的資料快取（含近 5 分鐘的 CPU/記憶體樣本）
internal/service/     背景 refresh loop（Refresher），定期呼叫 proxmox.Client 更新 cache
internal/tasks/       guest 電源操作的非同步 task 追蹤（送出 -> 監控 UPID -> 完成）
internal/httpapi/     所有 HTTP/WebSocket route、console/guest console 的 session 管理
frontend/              React + Vite 前端（唯一的瀏覽器端程式碼）
docs/                  這份技術文件（會被同步到 GitHub 公開 repo）
build.sh / deploy.sh    發布與 FreeBSD 部署腳本
```

## 執行期資料流

```text
              ┌────────────────────┐
              │   pve-web.yaml     │
              │ (targets 設定)     │
              └─────────┬──────────┘
                        │ config.Load()
                        v
┌───────────────────────────────────────────┐
│  runtime.State (internal/runtime)         │  <- 唯一持有 map[targetID]*proxmox.Client 的地方
└───────┬───────────────────────────┬───────┘
        │                           │
        v                           v
┌───────────────────┐      ┌─────────────────────────┐
│ service.Refresher  │      │ httpapi.Server          │
│ 每 N 秒對每個       │      │ 處理所有 /pve-web/* 路由 │
│ enabled target     │      │ (data/operation/console │
│ 呼叫一次 Proxmox   │      │  /console/guests/...)   │
│ API，寫入 cache    │      └───────────┬──────────────┘
└─────────┬──────────┘                  │
          v                              v
   ┌─────────────┐              瀏覽器（React frontend）
   │ cache.Store  │  <──── GET /pve-web/data/overview（前端每 5 秒 poll 一次）
   └─────────────┘
```

- `Refresher`（`internal/service/refresh.go`）是唯一會主動、定期呼叫 Proxmox API 的
  地方；前端**不會**直接打 Proxmox，永遠只打 `pve-web` 自己的 API，`pve-web` 再去問
  Proxmox、把結果整理進 `cache.Store`。
- 前端的「即時」感覺完全來自它每 5 秒重新 `fetch('/pve-web/data/overview')`；後端本身
  沒有 WebSocket 推送 dashboard 資料（WebSocket 只用在 Node Shell / Guest Console 兩個
  console 功能上，見 `docs/console-protocols.md`）。
- Guest 電源操作（start/shutdown/stop/reboot）是「送出後非同步監控」模式：
  `POST /pve-web/operation/guests/{target}/{type}/{vmid}` 送出 Proxmox 的操作後立刻
  回應一個 `job.id`，前端再輪詢 `GET /pve-web/tasks/{jobID}` 直到狀態變成
  `succeeded`/`failed`（`internal/tasks/tasks.go` 的 `Manager.monitor`）。

## 設定與 credential 分離

- `pve-web.yaml`：非敏感設定（server listen、refresh 間隔、logging、targets 的
  endpoints/type/id 等）。
- `pve-web-credentials.json`：敏感的 Proxmox API token、console 帳密，權限強制 `600`。
- `pve-web-credential.json`（注意沒有 `s`）：從舊版 `pve-console` TUI 匯出的**交換
  檔**，只在第一次啟動、`pve-web-credentials.json` 還不存在時被讀入並轉存；不是
  runtime 檔案，不會被程式持續使用。

兩者用 target ID 對應：`config.Target`（`pve-web.yaml`）描述「這個 target 長什麼樣
子」，`credentials.Credential`（`pve-web-credentials.json`）描述「這個 target 的
密碼是什麼」。`cmd/pve-web/main.go` 啟動時把兩者合併成
`map[string]*proxmox.Client`，之後全程式都透過這個 map 存取 Proxmox，不會有其他地方
重新讀取 credential 檔案。

## Console 功能為什麼要獨立一份文件

Node Shell 跟 Guest Console 用的是完全不同的 Proxmox API
（`termproxy` vs `vncproxy`），連底層的 WebSocket 端點、票證驗證方式都不一樣，過去
在這兩塊各踩過好幾個一開始很難察覺的 bug（idle timeout、API schema 差異、票證
authpath 不符、noVNC 鍵盤焦點）。這些細節單獨整理在
[`docs/console-protocols.md`](./console-protocols.md)，避免以後改動時重蹈覆轍。

## 版本與發布

- 版本號規則見 `AGENTS.md`（僅本地保存，未公開於此 repo）：任何會影響已上線行為的
  改動都必須升版。
- `build.sh` 負責建置 Go binary（依平台 cross-compile）、frontend（`npm run build`）、
  產生 `releases/{version}/` 發布包與 `releases/latest.json`，並把完整專案樹同步到
  部署鏡像目錄（不含 `.git`）。
- `deploy.sh` 是實際在 FreeBSD 主機上執行 install/upgrade/rollback 的腳本，會保留既有
  設定與 credential，不會被升級覆蓋。
