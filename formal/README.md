# Lean によるモデルと証明

このディレクトリには、agones-lite の小さな純粋関数を Lean 4 で書き直したモデルと、その性質の証明がある。
Go のテストは「試した入力で正しい」ことを示す。
ここでは「どんな入力でも正しい」ことを示す。

## 証明しているのは何か

証明の対象は Lean で書いたモデルで、Go のコードそのものではない。
モデルが Go のコードと違っていれば、証明は何も保証しない。
そこで、モデルを実行して入力と期待値の組を出力し、Go のテストがそれを読んで実装と突き合わせている。

| モデル | 対応する Go のコード | 出力する組 | 突き合わせるテスト |
|---|---|---|---|
| `Formal/Ports.lean` | `pkg/portallocator` | `vectors/ports.jsonl`（2,040 件） | `pkg/portallocator` |
| `Formal/Counts.lean` | `pkg/gameservers/counts.go`、`pkg/sdkserver/counts.go` | `vectors/counters.jsonl`（2,500 件） | `pkg/gameservers`、`pkg/sdkserver` |
| `Formal/StateMachine.lean` | `pkg/gameservers/state.go` | `vectors/transitions.jsonl`（187 件） | `pkg/gameservers` |
| `Formal/Rollout.lean` | `pkg/fleets/rollingupdate.go` | `vectors/rollout.jsonl`（1,984 件）、`vectors/stuck.jsonl` | `pkg/fleets` |

状態遷移表は、状態と出来事の組み合わせが 187 通りしかない。
その全部を出力しているので、テストが通れば、モデルと Go の表は完全に一致している。
ポート、Counter、ローリング更新は入力が無限にあるので、一致を確かめているのは出力した範囲に限られる。

## 何が分かったか

Lean の価値は、証明が通らない箇所にある。
証明に前提を足さなければならなかったところは、実装が暗黙に頼っている前提である。
その前提を誰も保証していなければ、不具合になる。

### ポートの割り当て

証明したこと（`Formal/Ports.lean`）：

- `pickFree_length`、`pickFree_free`、`pickFree_nodup`：選ばれたポートは、求めた個数で、ノードの範囲の中にあり、誰にも使われておらず、互いに重ならない。
- `pickFree_complete`：失敗するのは、範囲に空きが足りないときだけである。
- `allocate_distinct`：割り当ての後、1 つの GameServer のポートは互いに重ならず、他の GameServer のポートとも重ならない。

見つけたこと：

- `allocateBeforeTheFix_reuses_a_static_port`：以前の実装は、GameServer 自身の Static ポートを知らずに残りを選んでいた。Static ポートがノードの範囲の中にあると、同じ番号がもう一度配られる。Docker はこのコンテナを起動できない。
- `allocate_does_not_check_static_ports_against_each_other`：`allocate_distinct` には「Static ポートが互いに重ならない」という前提が要る。割り当ての関数はこれを調べないので、受け付けの段階で調べる必要がある。

モデルは、プロトコル（TCP と UDP）を区別しない。
同じ番号を TCP と UDP の両方で使う定義は正しいが、モデルでは重なりとして扱われる。

### Counter の更新

証明したこと（`Formal/Counts.lean`）：

- `counterAfter_in_range`：容量が負でない Counter は、どんな更新の後も、count が 0 以上で容量以下である。
- `accepted_is_not_cut`：sidecar が受け付けた要求は、count について何かを言っているかぎり、切り詰められずにそのまま保存される。ゲームサーバに返した値と、保存される値が食い違わない。
- `accepted_difference_did_not_wrap`：Go の加算は 64 ビットであふれる。count が負でなければ、あふれた結果は必ず負になるので、sidecar の検査を通らない。つまり、受け付けた差分は正確な値である。

見つけたこと：

- `counterAfter_with_a_negative_capacity`：容量が負の Counter は、更新のたびに count が負になる。
- `a_negative_count_fools_the_check`：count が負の Counter に大きな負の差分を足すと、あふれて大きな正の数になり、検査を通ってしまう。
- どちらも、「容量と count が負の Counter は存在しない」という前提が破れたときに起きる。この前提は、受け付けの検査が保証しなければならない。以前の実装は検査していなかった。
- `lowering_the_capacity_cuts_the_count`：容量だけを下げる要求は受け付けられ、count は新しい容量まで切り詰められる。これは本家と同じ仕様で、不具合ではない。

List の更新と、sidecar の予測がコントロールプレーンの反映と一致することは、ここでは扱っていない。
後者は Go のプログラム同士の比較なので、Go のテストで突き合わせている。

### 状態遷移表

証明したこと（`Formal/StateMachine.lean`）：

- `final_is_final`：終わった状態（Shutdown、Unhealthy、Error）からは、何が起きても動かない。
- `scaling_down_spares_sessions`：スケールダウンは、Allocated と Reserved の GameServer を動かさない。
- `what_ends_a_session`：Allocated の GameServer を別の状態に移せるのは、ゲームサーバ自身の要求と、障害だけである。コントロールプレーンの通常の制御では動かない。
- `never_stuck`：終わっていない GameServer は、いつでも終了させられ、いつでも失われたものとして扱える。行き止まりの状態はない。
- `every_state_is_reachable`：どの状態にも、最初の状態から到達できる。使われない行はない。

見つけたこと：

- `allocated_before_running`：ゲームサーバは、コンテナの起動が報告される前に Allocate を呼べる。そうなると、起動の報告は状態を変えない。以前の実装は「状態が Scheduled か Ready に変わったとき」にアドレスを公開していたので、この順序ではアドレスのない Allocated ができていた。
- `an_expired_reservation_ends_only_a_reservation`：Reserve の期限切れが動かせるのは、Reserved の GameServer だけである。以前は、期限が切れると sidecar が Ready を要求していた。同じ瞬間にゲームサーバが自分を Allocate すると、Allocated から Ready に戻されていた。期限切れを専用の出来事にして、この順序でも対戦が残るようにした。

### ローリング更新

証明したこと（`Formal/Rollout.lean`）：

- `grow_stays_within_the_surge`：新しい GameServerSet が増えるとき、Fleet 全体は、指定の台数を maxSurge を超えては上回らない。
- `grow_keeps_what_the_active_set_has`：Fleet が台数を超えていても、新しい GameServerSet は、いま持っている台数を減らされない。
- `grow_leaves_room_for_sessions`：新しい GameServerSet は、古い側に残っている Allocated の GameServer の分を空けておく。
- `grow_makes_progress`：maxSurge の範囲に余地があるかぎり、1 回の処理で必ず前に進む。
- `scaleDown_gives_up_the_limit_at_most`：古い GameServerSet から手放す GameServer は、合計で上限を超えない。対戦中のものしか残っていない GameServerSet を「済み」とすることは、何も手放さない。

見つけたこと：

- `the_floor_is_below_what_is_ready`：「最低限保つ Ready の台数」は、Fleet の台数からではなく、いま Ready の台数から計算される。そのため、下限はつねに現在の Ready の台数より下にあり、処理のたびに下がりうる。
- `a_version_that_never_gets_ready_takes_most_of_the_fleet`：モデルを実行して確かめた例である。100 台の Fleet に、Ready にならない版を maxUnavailable 25%、maxSurge 1 で配ると、Ready の台数は 75、57、43、33、25、19 と減り、19 台で止まる。

`grow` の 2 つの性質は、プロパティベーステストが反例を見つけて Go の実装を直した後に、証明できるようになった。
直す前の計算は本家と同じで、古い GameServerSet が台数に達していないときに maxSurge を超え、Fleet を途中で小さくすると新しい版の GameServer を捨てていた。

モデルは、GameServerSet の制御が「対戦中でないものから消す」ことを前提にしている。
台数の計算が 32 ビットに収まることも前提にしている。

## 使い方

Lean のツールチェーンは `elan` が管理する。
バージョンは `lean-toolchain` で固定している。
Mathlib は使っていない。

```sh
brew install elan-init   # macOS の場合
cd formal
lake build               # すべての証明を検査する
lake exe vectors         # モデルを実行して vectors/ を作り直す
```

モデルを変えたら `lake exe vectors` を実行し、`vectors/` の変更も一緒にコミットする。
Go のコードを変えてテストが落ちたら、モデルと実装のどちらが正しいかを決め、正しいほうに合わせる。

## 読む順序

1. `Formal/StateMachine.lean`：もっとも短い。表と、それについての定理。
2. `Formal/Ports.lean`：`pickFree` から読み、`allocate` に進む。
3. `Formal/Counts.lean`：`counterAfter` と `accepted` を読み、2 つが一致する条件を読む。最後に 64 ビットの節。
4. `Formal/Rollout.lean`：`growBy` と 4 つの定理を読み、`scaleDown` に進む。最後に下限の節。
5. `Vectors.lean`：モデルから組を作るプログラム。
