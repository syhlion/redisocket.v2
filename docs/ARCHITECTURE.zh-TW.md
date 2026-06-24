# redisocket.v2 架構

> 🌐 [English](ARCHITECTURE.md) · **繁體中文**

redisocket.v2 是 gusher.cluster 底下的 WebSocket hub 引擎。它持有 ws 連線、把訊息
fan-out 給 client,並把跨節點的**匯流排（bus）**與 **presence** 放在可替換的介面後
——一個 Redis 實作、一個 NATS 實作,換後端時連線層完全不用動。

> 圖的原始檔是 `diagrams/` 裡的 `.drawio`,用 draw.io 開啟可編輯。

## 引擎內部

![engine](diagrams/engine-internals.drawio.png)

三層:

- **連線層** — 每條 ws 連線兩個 goroutine:`readPump`(讀入)、`writePump`
  (寫出,select on `quit` 可優雅退出）。inbound 訊息經一個可設定大小的 worker
  pool（`messageQuene`）。
- **pool goroutine** — `users` map 的**唯一擁有者**;處理 broadcast / join /
  leave / kick、週期 `presence.Sync`、靠 `quit` channel 收尾。對每個 client 的送訊息
  **與**其關閉都在這裡序列化,所以不會有 send-on-closed-channel 的 race。
- **抽象層** — `dispatchLoop` 消費 `Broker.Subscribe()`;`Sender` 經
  `Broker.Publish()` 發佈;presence 走 `Presence` 介面。連線/pool/dispatch 的程式
  從不指名任何後端。

## Broker（跨節點匯流排）

`Broker`：`Publish(prefix, appKey, event, data)`、
`Subscribe(prefix) → (events, errors)`、`Close()`。

- **redisBroker** — Redis `PUBLISH` / `PSUBSCRIBE`。用 `pool.Dial()` 取**專用連線**
  （非 pooled）,讓 `Close` 能 race-free 地中斷阻塞中的 `Receive`。
- **natsBroker** — NATS subject `<prefix>ch.<appKey>`;event 名放在訊息 **header**、
  data 放 **body**（免編碼——頻道名含 `.` 或特殊字元都安全）。`ChanSubscribe` +
  `done` channel 乾淨停止。
- 收端統一:`dispatchLoop` 拿到 `BrokerEvent{appKey, event, data}` →
  `handleEvent`（`#GUSHERFUNC-*#` 控制事件,其餘為頻道廣播）。

## Presence

`Presence`：`Touch`、`Sync`（批次快照）、`Online` / `OnlineByChannel` /
`Channels`、`Close`。

- **redisPresence** — sorted set（`ZADD` score=時戳、`ZRANGEBYSCORE`、週期
  `ZREMRANGEBYSCORE`）。全域共享 store、強一致。
- **memoryPresence** — per-node 記憶體狀態;跨節點查詢走 NATS **request/reply**
  scatter-gather（每個節點回報自己的連線、結果合併）。無 store、無 `KEYS`。查詢時
  即時聚合（最終一致）——在 10 萬+ 連線下是更好的設計。

## 優雅關閉

`Hub.Close()`（冪等）關閉 `quit` channel 與 broker。`pool.run`、message workers、
`statistic.Run`、每條 client 的 `writePump`、broker 訂閱全部退出;
`go.uber.org/goleak` 驗證 **零 goroutine 洩漏**。

## 測試

同一套行為測試對三個後端各跑一次——**redis**（miniredis）、**nats**（內嵌
`nats-server`）、**nats-native**（natsBroker + memoryPresence,零 Redis）——全程
`-race`、完全 in-process（不用 Docker）。並涵蓋跨節點 presence 聚合、含點頻道的
subject 映射、`goleak`。

## 延伸

- [gusher.cluster 架構](https://github.com/syhlion/gusher.cluster/blob/master/docs/ARCHITECTURE.zh-TW.md)
  — 內嵌此引擎的系統
- `logger.go` — opt-in `NewLogger`:輸出 **stdout / file / both** + 日誌輪替
  （lumberjack）;引擎本體只吃 `*slog.Logger`
