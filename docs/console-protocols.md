# Console 協定筆記：Node Shell 與 Guest Console

這份文件記錄 `pve-web` 的兩種 console 功能（Node Shell、Guest Console）背後實際使用的
Proxmox VE API，以及開發過程中踩到的幾個坑與根因。內容都是直接對照 Proxmox 官方原始碼
（`git.proxmox.com`）驗證過的，不是猜測。之後要修改 console 相關程式碼前，建議先讀完
這份文件。

## 兩種 console，兩套完全不同的機制

| | Node Shell | Guest Console（VM/CT） |
|---|---|---|
| 用途 | 登入 Proxmox 節點本身的 shell | 連到 VM/CT 的畫面或文字終端機 |
| 建立 session | `POST /nodes/{node}/termproxy` | `POST /nodes/{node}/{qemu\|lxc}/{vmid}/vncproxy` |
| WebSocket 端點 | `GET /nodes/{node}/vncwebsocket` | `GET /nodes/{node}/{qemu\|lxc}/{vmid}/vncwebsocket` |
| 票證 authpath | `/nodes/{node}` | `/vms/{vmid}` |
| 前端協定 | termproxy 多工文字協定（xterm.js） | 原生 RFB/VNC bytestream（noVNC） |
| 後端行程 | `/usr/bin/termproxy`（無 idle timeout） | QEMU：QEMU 自帶 VNC server（無 idle timeout）<br>LXC：`/usr/bin/vncterm -timeout 10`（**有 10 秒 idle timeout**） |

程式碼對應：

- `internal/proxmox/client.go`：`NodeTermProxy`/`DialNodeTermProxy`（Node Shell）、
  `GuestVNCProxy`/`DialGuestVNC`（Guest Console）、共用的底層 dial helper
  `dialVNCWebsocket`。
- `internal/httpapi/server.go`：`console()`/`consoleWebsocket()`（Node Shell）、
  `guestConsole()`/`guestConsoleWebsocket()`（Guest Console）、共用的
  `relayWebsocket()`。
- `frontend/src/main.tsx`：Node Shell 用 `@xterm/xterm`（動態載入），Guest Console 用
  `@novnc/novnc`（動態載入）。

## 為什麼 Node Shell 以前會卡在「閒置 10 秒斷線」

舊版 Node Shell 用的是 Proxmox 的 `vncshell` API（`PVE::API2::Nodes::vncshell`），它會
啟動：

```perl
my $timeout = 10;
my $cmd = ['/usr/bin/vncterm', '-rfbport', $port, '-timeout', $timeout, ...];
```

`vncterm` 內部的 idle timer **只有真正的鍵盤/滑鼠輸入寫進 pty**（`vt->ibuf_count > 0`）
才會重置，任何 WebSocket 層的心跳/keepalive 都救不了它——這是 Proxmox 自己刻意的設計，
不是 bug，也沒有官方參數可以關掉。

修法：改用 Proxmox 的 `termproxy` API（`/usr/bin/termproxy`），這支程式**完全沒有
idle timeout**，前端改用真正的 `xterm.js` 終端機。這個改動在 v0.4.13 完成，Node Shell
從此不會再因為閒置而斷線。

## Guest Console：QEMU 與 LXC 的 API schema 不一樣

在幫 Guest Console 補上 `vncproxy` 呼叫時，一開始沿用了跟 Node Shell 類似的參數
（帶 `width`/`height`），結果 QEMU 直接回 HTTP 400：

```json
{"errors":{"width":"property is not defined in schema and the schema does not allow additional properties","height":"..."}}
```

對照 Proxmox 的 `pve-manager.git`/`pve-container.git` 原始碼，QEMU 跟 LXC 的
`vncproxy` 參數表其實不一樣：

- `/nodes/{node}/qemu/{vmid}/vncproxy`：只接受 `node`、`vmid`、`websocket`、
  `generate-password`。**不接受 `width`/`height`**——因為 QEMU 用的是自己內建的真正
  VNC server，畫面尺寸由 VM 的顯示裝置決定，跟這個 API 無關。
- `/nodes/{node}/lxc/{vmid}/vncproxy`：額外接受 `width`（16–4096）、`height`
  （16–2160）——因為 LXC 容器沒有真正的圖形畫面，這兩個參數是用來告訴底層的
  `vncterm` 要畫多大的文字終端機框。

`internal/proxmox/client.go` 的 `GuestVNCProxy` 現在只在 `guestType == "lxc"` 時才帶
`width`/`height`。

## Guest Console 開了幾秒就斷線：連錯 vncwebsocket 端點

修完上面的 400 之後，實測發現 console 視窗一開沒幾秒就跳「WebSocket disconnected」，
畫面自動關掉。追下去才發現：Proxmox 其實註冊了**三個完全獨立**的 `vncwebsocket`
端點，各自用不同的 authpath 驗證票證：

```text
/nodes/{node}/vncwebsocket                     -> 驗證 authpath /nodes/{node}   (只給 Node Shell 用)
/nodes/{node}/qemu/{vmid}/vncwebsocket         -> 驗證 authpath /vms/{vmid}     (QEMU guest console)
/nodes/{node}/lxc/{vmid}/vncwebsocket          -> 驗證 authpath /vms/{vmid}     (LXC guest console)
```

而 Guest Console 的 `vncproxy` 呼叫產生的票證，是用 `/vms/{vmid}` 這個 authpath 簽署的
（可在 `PVE::API2::LXC` 的 `vncproxy` 實作裡看到 `my $authpath = "/vms/$vmid";`）。

問題出在 `internal/proxmox/client.go` 的 `dialVNCWebsocket`：不管是 Node Shell 還是
Guest Console，一律連到第一個（節點層級）端點。Guest Console 的票證拿去那裡驗證，
Proxmox 的 `verify_vnc_ticket()` 會因為 authpath 不符而直接拒絕，WebSocket handshake
失敗，噴出的就是「連線建立後幾秒內斷線」——跟 reverse proxy 設定完全無關。

修法：`dialVNCWebsocket` 改成吃完整路徑參數；`DialGuestVNC` 改連對應的
`{node}/{qemu|lxc}/{vmid}/vncwebsocket`（`GuestVNC` 因此多了 `GuestType`/`VMID`
兩個欄位純粹是為了組這個路徑用，不是 Proxmox API 回應的一部分）；`DialNodeTermProxy`
維持原樣（本來就是對的）。

## Guest Console 鍵盤：按 Enter 就斷線

修完上面兩個之後，又發現：QEMU console 開啟後，如果不先用滑鼠點一下畫面，按下 Enter
（或 Space）會讓 console 直接斷線、視窗關掉。

根因是 noVNC 的鍵盤焦點行為（`@novnc/novnc` `core/rfb.js`）：

```js
// 只有滑鼠按下/觸控才會讓 canvas 搶到鍵盤焦點
this._canvas.addEventListener("mousedown", this._eventHandlers.focusCanvas);
this._canvas.addEventListener("touchstart", this._eventHandlers.focusCanvas);
```

Console 是點擊 guest 列表裡的「⌘」按鈕開啟的。點擊一個 `<button>` 會讓瀏覽器把鍵盤
焦點留在那顆按鈕上，而且**不會**因為畫面上多疊了一層 modal 就自動移走。noVNC 的
canvas 在連線成功後也不會主動搶焦點，只有使用者自己點擊/觸碰畫面才會。於是：

- 焦點還停留在「⌘」按鈕上時，所有鍵盤輸入都打不進 VM。
- 按下 **Enter/Space**：瀏覽器對「目前有焦點的 `<button>`」的預設行為就是觸發它的
  click——等於重新呼叫了一次 `openGuestConsole(g)`，把剛開的 session 拆掉、再開一個
  新的。這就是「按 Enter 就斷線、視窗關掉」的真正原因。

修法：在 `frontend/src/main.tsx` 的 Guest Console effect 裡，監聽 RFB 的 `'connect'`
事件並呼叫 `rfb.focus()`，跟 Node Shell 那邊連線成功後呼叫 `term.focus()` 的做法一致。
連線一成功就把鍵盤焦點主動移到 VNC 畫面，不需要使用者先手動點擊，也不會再被 Enter
誤觸還留著焦點的按鈕。

## 已知但尚未修的潛在問題

- **LXC 的 `vncproxy` 仍然是 `vncterm -timeout 10`**：跟舊版 Node Shell 一樣的
  idle-timeout 二進位檔。目前沒有實際症狀（因為之前遇到的斷線都是上面那兩個 bug造成
  的，都在幾秒內發生，不是 10 秒閒置），但如果之後真的觀察到 LXC console **閒置約
  10 秒**會斷線，可以考慮把 LXC guest console 也從 `vncproxy`/noVNC 換成
  `termproxy`/xterm.js（LXC 一樣有 `/nodes/{node}/lxc/{vmid}/termproxy`，沒有 idle
  timeout），做法跟 Node Shell 在 v0.4.13 的遷移完全一樣。QEMU 沒有這個風險，因為它
  接的是 QEMU 自己的 VNC server，不會經過 `vncterm`。
- Console session（`s.sessions`/`s.guestSessions`）目前沒有背景 GC，只在被使用時檢查
  是否過期；長時間執行的 process 裡，被放棄未使用的 session 會一直留在記憶體，直到
  process 重啟。目前影響有限（session 只存 client 指標、node、票證等小資料），但這是
  已知的技術債。

## 除錯技巧

- 後端在建立 session 與 dial websocket 的每一步都有 log（見
  [`docs/logging.md`](./logging.md)），格式固定帶 `target=`/`node=`/`type=`/`vmid=`，
  絕對不會印出票證、密碼本身，只會印 `ticket_present=`/`ticket_len=`。
- `relayWebsocket` 現在會記錄「哪一邊先關閉連線、什麼錯誤」，任何 console 連線中途
  斷掉都會在 `pve-web.log` 留下痕跡，不會無聲消失。
- 如果 Guest Console 打不開，先看是 HTTP 層就失敗（`guest console start failed`，
  通常是 Proxmox API schema/權限問題）還是 WebSocket 層斷線（`guest console websocket
  rejected`/`relay ended`，通常是票證 authpath 或 reverse proxy 設定問題）。
