# Log 檔與問題診斷

`pve-web` 的所有 log 都是透過 Go 標準函式庫的 `log` package 輸出，格式是固定的
`key=value` 風格，方便用 `grep`/`awk` 篩選，也方便直接貼給別人看而不需要額外解釋。

## Log 輸出到哪裡

`pve-web.yaml` 裡的 `logging` 區塊決定 log 要寫到哪裡：

```yaml
logging:
  enabled: true
  file: /var/log/pve-web/pve-web.log
```

- `enabled: true`（預設）：所有 log（包含 `log.Fatal`）都會寫進 `file` 指定的檔案，
  以 append 模式開啟（`O_CREATE|O_APPEND`），權限 `0640`。**不會**同時印到終端機的
  stdout/stderr——這是刻意的設計，避免跟 FreeBSD `daemon(8)` 的 `-o` 重導向重複寫入
  同一個檔案兩次（見下方「FreeBSD 部署細節」）。
- `enabled: false`：維持 Go 預設行為，log 印到 stderr。適合本機開發測試時直接在終端機
  看輸出。
- 如果設定的 `file` 路徑打不開（權限不足、目錄不存在等），`pve-web` 會印一行警告到
  stderr 並自動退回預設輸出，**不會**因為 log 檔開不了就啟動失敗。

時間戳記精度到微秒（`log.Lmicroseconds`），方便在同時有多個 console session、多個
target 在並行 refresh 時，還能準確分辨事件發生的先後順序。

## FreeBSD 部署細節

`pve_web.in`（rc.d script）用 `daemon -o /var/log/pve-web/pve-web.log` 啟動
process。這個 `-o` 本來是給「完全不知道自己在被 daemon 化、只會寫 stdout/stderr」的
程式用的重導向機制。`pve-web` 從 v0.4.18 開始會自己直接開檔寫入同一個路徑，所以
process 本身的 stdout/stderr 實際上不會再有內容——`daemon -o` 的重導向變成無害的
空操作，log 內容只會出現一次，不會重複。

如果之後要調整 rc.d script 或改用其他 process supervisor，只要那個 supervisor不會
主動清空/truncate log 檔案本身即可；`pve-web` 開檔時用的是 append 模式，不會覆寫舊
內容。

## 會被記錄的事件

| 事件 | 觸發時機 | 範例 |
|---|---|---|
| target refresh 開始失敗/恢復 | `internal/service/refresh.go`，每個 target 的錯誤**狀態變化**時（不是每次 poll 都印，避免灌爆 log） | `target refresh failed target=prod-cluster kind=authentication error=authentication failed` / `target refresh recovered target=prod-cluster` |
| guest 電源操作送出/結束 | `internal/tasks/tasks.go` | `guest power submitted target=... type=qemu vmid=101 action=start ...` / `guest power finished id=job-3 target=... type=qemu vmid=101 action=start status=succeeded message="Proxmox task completed"` |
| Node Shell session 建立/失敗 | `internal/httpapi/server.go` `console()` | `node console start target=... node=pve01` / `node console start failed ... error=...` |
| Node Shell WebSocket 建立/被拒/中斷 | `consoleWebsocket()`、`relayWebsocket()` | `node console websocket rejected node=pve01 session_found=false reason=unknown_session` / `node console node=pve01 relay ended: browser closed the connection: ...` |
| Guest Console session 建立/失敗 | `guestConsole()` | `guest console start target=... node=... type=qemu vmid=101` |
| Guest Console WebSocket 建立/被拒/中斷 | `guestConsoleWebsocket()`、`relayWebsocket()` | `guest console node=... type=qemu vmid=101 relay ended: proxmox closed the connection: ...` |

`relay ended` 的訊息會明確指出是「browser 端關閉」還是「proxmox 端關閉」，以及底層
的 error（例如 TCP 被重設、TLS handshake 失敗等），這樣看 log 的人不需要再去猜是
瀏覽器端還是 Proxmox 端先斷的。

## 絕對不會被記錄的內容

依照 `AGENTS.md` 的規範，以下內容永遠不會出現在 log 裡：

- Console 密碼（`console_password`）、VNC ticket/password 本身
- API token value
- PVEAuthCookie 值
- noVNC 密碼

需要判斷「有沒有票證」時，log 只會印 `ticket_present=true/false` 和
`ticket_len=<數字>`，不會印票證內容本身。

## 常見問題怎麼從 log 判斷

1. **Dashboard 資料一直不更新/顯示某個 target 錯誤**：`grep "target refresh" pve-web.log`，
   看 `kind=` 是 `authentication`（token 失效）、`permission`（權限不足）、
   `connection`（網路/TLS 打不通）還是 `configuration`（target 沒設定 credential）。
2. **Console 開不起來，HTTP 階段就失敗**：找 `console start failed`/
   `guest console start failed`，`error=` 後面通常會是 Proxmox API 回的訊息（例如
   schema 錯誤、權限不足）。
3. **Console 視窗跳出來又馬上消失**：找 `websocket rejected`（session 找不到/過期/
   node 不符）或 `relay ended`（連線建立後才斷）。`relay ended` 的訊息會說是哪一邊
   先關閉、什麼錯誤，通常就能判斷是 reverse proxy WebSocket 設定問題、票證問題，還
   是 Proxmox 端把連線斷掉。
4. **Guest 電源操作沒有反應/失敗**：`grep "guest power" pve-web.log`，`submitted` 跟
   `finished` 兩行的 `id=` 可以對起來看整個操作的開始跟結束狀態。
