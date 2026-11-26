# モデルによる証明と検査

このディレクトリには、scrim の一部を書き直したモデルが 2 種類ある。

- **Lean のモデル**（`Formal/`）：小さな純粋関数を Lean 4 で書き直し、性質を証明する。
- **Quint のモデル**（`quint/`）：状態の移り変わりを Quint で書き直し、到達できる状態をモデル検査器ですべて調べる。

Go のテストは「試した入力と、試した順序で正しい」ことを示す。
Lean の証明は「どんな入力でも正しい」ことを示し、Quint の検査は「出来事がどんな順序で起きても正しい」ことを示す。

## Lean で証明しているのは何か

証明の対象は Lean で書いたモデルで、Go のコードそのものではない。
モデルが Go のコードと違っていれば、証明は何も保証しない。
そこで、モデルを実行して入力と期待値の組を出力し、Go のテストがそれを読んで実装と突き合わせている。

| モデル | 対応する Go のコード | 出力する組 | 突き合わせるテスト |
|---|---|---|---|
| `Formal/Ports.lean` | `pkg/portallocator` | `vectors/ports.jsonl`（2,040 件） | `pkg/portallocator` |
| `Formal/Counts.lean` | `pkg/gameservers/counts.go`、`pkg/sdkserver/counts.go` | `vectors/counters.jsonl`（2,500 件） | `pkg/gameservers`、`pkg/sdkserver` |
| `Formal/StateMachine.lean` | `pkg/gameservers/state.go` | `vectors/transitions.jsonl`（198 件） | `pkg/gameservers` |
| `Formal/Rollout.lean` | `pkg/fleets/rollingupdate.go` | `vectors/rollout.jsonl`（1,984 件）、`vectors/stuck.jsonl` | `pkg/fleets` |

状態遷移表は、状態と出来事の組み合わせが 198 通りしかない。
その全部を出力しているので、テストが通れば、モデルと Go の表は完全に一致している。
ポート、Counter、ローリング更新は入力が無限にあるので、一致を確かめているのは出力した範囲に限られる。

## Lean の証明で分かったこと

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
- `a_request_gets_only_what_can_take_a_session`：Allocation API がマッチメイカに渡すのは、Ready か Allocated の GameServer だけである。起動の途中のもの、ゲームサーバが Reserve で押さえているもの、終わったものは渡さない。
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

## Quint で検査しているのは何か

状態の移り変わりは、Go のコードでは 1 つの表にまとまっているとは限らない。
GameServer の状態は `pkg/gameservers/state.go` の表にあるが、ほかの多くは、真偽値の組み合わせ、`nil` かどうか、タイマー、関数の何行目まで進んだか、といった形で持たれている。
Quint のモデルは、それらを状態と出来事として書き出したものである。

モデル検査器には TLC を使う。
TLC は、モデルが到達できる状態を 1 つ残らずたどり、不変条件が破れる状態がないことと、公平性の仮定のもとで「いずれ起きるはずのこと」が起きることを確かめる。
回数の上限（SDK の呼び出しは 3 回まで、など）はモデルごとに決めてあり、状態の数は有限である。

| モデル | 対応する Go のコード | 状態の数 |
|---|---|---|
| `quint/gameserver.qnt` | `pkg/gameservers`（`state.go`、`lifecycle.go`、`controller.go`） | 744 |
| `quint/requests.qnt` | `pkg/sdkserver`（`sdk.go`、`agent.go`）、`pkg/gameservers/controller.go` | 18,720 |
| `quint/link.qnt` | `pkg/nodes`（`remote.go`、`server.go`） | 1,874 |
| `quint/agent.qnt` | `pkg/agent`（`plan.go`、`agent.go`） | 11,305 |
| `quint/node.qnt` | `pkg/nodes`（`server.go`、`local.go`、`controller.go`）、`pkg/apis/scrim/v1/node.go` | 510 |
| `quint/queue.qnt` | `pkg/queue` | 3,688 |
| `quint/store.qnt` | `pkg/store/save.go` | 56,255 |
| `quint/cascade.qnt` | `pkg/fleets`、`pkg/gameserversets`、`pkg/gameservers` の削除 | 414 |
| `quint/health.qnt` | `pkg/sdkserver/health.go` | 108 |
| `quint/view.qnt` | `pkg/sdkserver/view.go` | 3,278 |

それぞれのモデルが調べていることは次のとおりである。

- **GameServer の一生**（`gameserver.qnt`）：制御のループ、エージェント、sidecar、Allocation API、運用者、ノードの切断と削除が、どんな順序で出来事を報告しても、終わった状態からは動かないこと、削除の途中の記録は状態を変えず元にも戻らないこと、対戦を終わらせるのはゲームサーバ自身の要求と障害と運用者の削除だけであること、プレイヤーを送れる状態にはアドレスがあること。ノードが切断と復帰を繰り返しても、削除はいずれ終わること。
- **SDK の要求の配送**（`requests.qnt`）：ゲームサーバの呼び出し、Reserve のタイマー、応答の紛失と送り直し、制御のループ、Allocation がどう入り混じっても、同じ要求が 2 度適用されないこと、Reserve の期限切れが対戦を終わらせないこと、期限切れの知らせが新しい Reserve の後ろに並ばないこと。
- **エージェントの手元の写し**（`link.qnt`）：GameServer の作成、変更、削除、同じ名前での作り直しと、接続の切断と再接続がどう入り混じっても、エージェントが従う写しは「ある時点の store の内容」であり、遅れることはあっても、巻き戻ることはないこと。
- **コンテナの起動と撤去**（`agent.qnt`）：エージェントの異常終了、Docker の失敗、報告の紛失、写しの遅れ、同じ名前での作り直しがどう入り混じっても、問題なく動いているコンテナを止めないこと、Docker が失敗していないのに「作成できなかった」と報告しないこと、他の GameServer のために作られたコンテナを起動しないこと。障害がやめば、いずれ落ち着くこと。
- **ノードの記録**（`node.qnt`）：登録、Watch の開始と終了、運用者の操作と削除、コントロールプレーンの再起動がどう入り混じっても、ノードが Ready なのは Watch が開いているときだけであること、切断中のノードの「最後に連絡があった時刻」が登録や再起動で先送りされないこと。
- **作業キュー**（`queue.qnt`）：キーの追加と、ワーカーが眠りに入る瞬間がどう重なっても、待っているキーがあるのにワーカーが眠り続けないこと。追加されたキーは、いずれ処理されること。
- **store のファイル**（`store.qnt`）：書き込み、背後での保存、保存の失敗、`Sync` の各段階、異常終了がどう入り混じっても、`Sync` が返した変更はファイルにあること（異常終了が取り消すのは、外に伝えていない変更だけであること）、待っている `Sync` が保存を見逃して眠り続けないこと。
- **連鎖削除**（`cascade.qnt`）：Fleet の削除と、Fleet、GameServerSet、GameServer の制御がどう入り混じっても、親より後に残る子がないこと、削除を求められた Fleet は対戦中の GameServer があってもいずれ消えること。
- **ヘルス判定**（`health.qnt`）：判定が始まる前、初期の猶予の間、無効のときには何も報告しないこと、報告は閾値の回数だけ続けて ping が途絶えた後に 1 度だけであること。
- **SDK に見せる GameServer**（`view.qnt`）：エージェントからの配信、変更への応答、応答の紛失がどう入り混じっても、送信中の変更を 2 度数えず、1 つも落とさないこと。

### Go のコードとの対応

検査の対象はモデルであり、Go のコードそのものではない。
対応の確かめ方は、モデルの部分によって違う。

モデルのうち、状態と出来事から次の状態を決める表は、Go の関数を写したものである。
`build/ci/models.sh vectors` は、これらの表を、ありうる入力のすべてについて `vectors/quint/` に書き出す。
Go のテストがそれを読み、実装と突き合わせる。
入力を 1 つも省いていないので、テストが通れば、モデルの表と Go の関数は同じ関数である。

| モデルの表 | 対応する Go の関数 | 出力する組 |
|---|---|---|
| `nextState` | `gameservers.Next` | `transitions.jsonl`（220 件） |
| `nextStage` | `gameservers.NextStage` | `stages.jsonl`（40 件） |
| `decide` | `gameservers.decide` | `decisions.jsonl`（88 件） |
| `wantOf` | `agent.wantOf` | `wants.jsonl`（22 件） |
| `conditionOf` | `agent.conditionOf` | `conditions.jsonl`（24 件） |
| `plan` | `agent.planOne` | `plans.jsonl`（1,056 件） |
| `dutyOf` | `Node.Duty` | `duties.jsonl`（4 件） |
| `operate`、`ask` | `nodes` の `operations`、`Node.Ask` | `operations.jsonl`（12 件） |

残りの部分、つまり「誰がいつ何を報告しうるか」と、並行処理の各段階の切り方は、Go のコードを読んで書いた仮定である。
この部分がコードと食い違っていれば、検査は何も保証しない。
各モデルの動作には、対応する Go の関数の名前をコメントで書いてある。

### 表に書き直したものと、モデルにだけ書いたもの

モデルを書く前に、暗黙に持たれていた状態のうち次の 4 つは、Go のコードのほうを表に書き直した。

- **Allocation API による割り当て**：以前は、状態の表を通さずに `Allocated` を書き込んでいた。出来事 `Allocated` として表に加え、割り当ての候補かどうかも表で判定する。
- **GameServer の記録の段階**：`deletionTimestamp` の有無を各所で調べていた。段階（Live、Leaving、Gone）と、それを進める 3 つの出来事の表にした（`lifecycle.go`）。制御のループの判断は、次に報告する出来事を返す。
- **ノードに運用者が求めること**：2 つの真偽値の組み合わせだった。Open、Cordoned、Draining の 3 つにし、操作ごとの行き先を表にした。
- **エージェントの判断**：6 つの関数に分かれた条件分岐だった。GameServer がコンテナに求めること、コンテナの状況、その組み合わせへの対処、の 3 つに分けた（`plan.go`）。

そのほかは、Go のコードを変えていない。
Reserve のタイマー、ヘルス判定、SDK に見せる GameServer、作業キュー、store の保存、エージェントの写しは、どれも 1 つの構造体の中に閉じていて、状態を列挙型に置き換えても読みやすくならないためである。
これらは、モデルの側で状態に名前を付けた。

ローリング更新の 1 回分は Lean のモデルが、Allocation のキーはコントロールプレーンのモデルテストが扱っているので、Quint のモデルは作っていない。

## Quint の検査で分かったこと

TLC が反例を見つけた不具合は 2 つある。
どちらも Go のコードを直し、直す前の挙動のモデルも残した。
`build/ci/models.sh` は、直す前のモデルで反例が見つかることも確かめる。
反例を示せなくなったモデルは、もう何も検査していないためである。

### 応答を失った要求が 2 度適用される

sidecar は、エージェントから応答が返らなかった要求を、返るまで送り直す。
応答が返らないのは、要求が届かなかったときだけではない。
要求は届いて適用され、応答だけが失われることがある。
エージェントやコントロールプレーンが応答の前に再起動したときと、ディスクが 10 秒を超えて止まったときに起こりうる。

`requestsUnnamed` を検査すると、TLC は次の 4 手の反例を返す。

1. ゲームサーバが Ready を呼ぶ。
2. 要求がコントロールプレーンに届き、RequestReady になる。応答は失われる。
3. 制御のループが Ready にする。
4. 同じ要求がもう一度届き、Ready から RequestReady に戻る。

3 と 4 の間に Allocation API がこの GameServer をマッチメイカに渡していると、4 は Allocated を RequestReady に戻す。
プレイヤーがいる GameServer が待機に戻り、次のマッチメイカに渡される。

そこで、sidecar は要求のすべてに名前を付け、コントロールプレーンは最後に適用した要求の名前を GameServer のアノテーション（`scrim/last-request`）に残す。
同じ名前の要求は適用しない。
カウンタとリストの変更には、同じ仕組みがすでにあった。
`requestsNamed` は、名前のある要求では、18,720 の状態のどれでも 2 度目の適用が起きないことを示す。

### 一覧より前の変更が、一覧の後ろから届く

コントロールプレーンは、エージェントの Watch に、ノードにある GameServer の一覧を送り、続けて変更を送る。
以前は、変更の購読を始めてから一覧を読んでいた。
その間に起きた変更は、一覧に含まれたうえで、一覧の後ろからもう一度届く。

`linkApart` を検査すると、TLC は 5 手の反例を返す。
エージェントが従う写しに、すでに消えた GameServer が現れる。
同じ名前で作り直された GameServer があると、古いほうの削除が後から届いて、新しいほうが写しから消える。
エージェントは、写しにない GameServer のコンテナを撤去する。

そこで、一覧の読み取りと購読の開始を、store の 1 回のロックの中で行うようにした（`store.OnNodeAndWatch`）。
`linkTogether` は、こうすると写しが巻き戻らないことを示す。

### 確かめられたこと

不具合ではなかったが、検査によって根拠が付いたものもある。

- Reserve のタイマーは、止めるのが間に合わなくても、ゲームサーバの次の要求を追い越さない。
- 作業キューと store の `Sync` には、起こす合図を取りこぼす順序がない。どちらも、合図の送り手と受け手がロックを共有していない箇所である。
- 削除の完了には、強い公平性が要る。ノードが切断と復帰を繰り返すと、「エージェントが撤去を報告する」も「制御が待つのをやめる」も、有効な状態が続かないためである。どちらも有効になるたびにいずれ実行されるなら、削除は終わる。

### モデルが扱っていないこと

- 時間は、順序としてだけ扱っている。5 分や 24 時間の猶予の長さは、モデルにない。
- GameServer、ノード、Fleet は、それぞれ 1 つか 2 つである。台数に関わる性質（ローリング更新の上限など）は、Lean のモデルと性質テストが扱う。
- エージェントのモデルでは、紛失した報告は届かなかったものとして扱う。届いたのに応答だけが失われる場合は、SDK の要求についてだけ `requests.qnt` が扱う。

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

Quint は、`quint/package-lock.json` に書いたバージョンを npm が取得する。
Node.js と jq のほかに、バージョン 21 以降の Java が要る。
モデル検査器は Quint が初回に `~/.quint` へ取得する。

```sh
build/ci/models.sh            # すべてのモデルを検査する（make models でも同じ）
build/ci/models.sh vectors    # モデルの表を実行して vectors/quint/ を作り直す
```

モデルを 1 つだけ調べるときは、Quint を直接呼ぶ。

```sh
cd formal/quint
node_modules/.bin/quint test requests.qnt --main requestsNamed
node_modules/.bin/quint verify requests.qnt --main requestsUnnamed --backend tlc --invariant aRepeatChangesNothing
```

2 つ目のコマンドは、直す前の挙動の反例を、状態の列として表示する。

モデルを変えたら、`lake exe vectors` か `build/ci/models.sh vectors` を実行し、`vectors/` の変更も一緒にコミットする。
Go のコードを変えてテストが落ちたら、モデルと実装のどちらが正しいかを決め、正しいほうに合わせる。

## 読む順序

Lean のモデルは、次の順に読む。

1. `Formal/StateMachine.lean`：もっとも短い。表と、それについての定理。
2. `Formal/Ports.lean`：`pickFree` から読み、`allocate` に進む。
3. `Formal/Counts.lean`：`counterAfter` と `accepted` を読み、2 つが一致する条件を読む。最後に 64 ビットの節。
4. `Formal/Rollout.lean`：`growBy` と 4 つの定理を読み、`scaleDown` に進む。最後に下限の節。
5. `Vectors.lean`：モデルから組を作るプログラム。

Quint のモデルは、どれも同じ並びで書いてある。
状態、動作、不変条件、いずれ起きるはずのこと、最後に読み物としての履歴（`run`）である。
履歴から読むと、モデルが何を再現しようとしているかが先に分かる。

1. `quint/gameserver.qnt`：前半の `gameserverTables` が Go の表の写しで、ほかのモデルもこれを使う。
2. `quint/requests.qnt` と `quint/link.qnt`：不具合を見つけた 2 つ。直す前と後を、定数 1 つで切り替えている。
3. `quint/agent.qnt`：前半の `agentTables` が `plan.go` の写しである。
4. `quint/queue.qnt` と `quint/store.qnt`：ロックを共有しない 2 者の合図を、1 段階ずつ分けて書いたもの。
5. `quint/node.qnt`、`quint/cascade.qnt`、`quint/health.qnt`、`quint/view.qnt`：どれも短い。
6. `quint/vectors.qnt`：表を書き出すためのモジュール。
