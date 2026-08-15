# Windows CI・push・release指針

この文書は、Windows Desktop版のpush後にGitHub Actionsが継続して失敗したv0.2.0事例をもとに、同じ問題を再発させないための必須手順を定める。

## 1. v0.2.0で何が起きたか

v0.2.0のsource、Setup.exe、checksumの公開自体は成功したが、そのpushで起動したWindows Gateは複数回失敗した。原因は製品のルーティング回帰ではなく、cleanなhosted Windows runnerとローカル環境の差をCI・test fixtureが正しく扱っていなかったことだった。

### 1.1 Wails frontend embedがGo jobに存在しなかった

`desktop-app/main.go`は`//go:embed frontend/*`で`desktop-app/frontend/dist`を必要とする。一方、当時のworkflowは次の構造だった。

- `Desktop frontend (Windows)` jobは`npm run build:web`を実行して`desktop/dist`を生成した。
- `Go tests (Windows)` jobはcheckout後すぐ`go test ./...`を実行した。
- job間にartifactのupload／downloadはなかった。

GitHub Actionsの各jobは独立したfilesystemを使う。別jobのbuild成果物は自動では引き継がれない。また、trackedな`desktop-app/frontend/dist/.gitkeep`はdotfileなのでGo embed対象にならず、directoryにはembeddable fileがないと判定された。

ローカルでは過去のWails buildでignoredな`dist`が残っていたため、この欠陥を見逃した。

### 1.2 Windowsの8.3短縮pathをredirectと誤判定した

GitHub hosted runnerは一時directoryを`C:\Users\RUNNER~1\...`のような8.3短縮pathで返すことがある。物理解決後は同じdirectoryが長い名前で返るため、単純な文字列比較では異なるpathに見える。

この差により次が発生した。

- Recoveryのancestor検査が、同じ通常directoryをjunction redirectと誤判定した。
- Traffic testのbackup directoryが、`LOCALAPPDATA`のtrusted base外にあると誤判定された。
- launcher testが、terminalから得た長いcwdとfixtureの短縮cwdを不一致と判定した。

対策ではWindowsの8.3表記だけを長い表記へ展開した。junctionやsymlinkを同一pathとして許可してはならない。reparse、DACL、volume identity、trusted-baseの安全契約は維持する。

### 1.3 hosted runner上のpackage並列実行でintegration testが飢餓状態になった

全Go packageを既定の並列度で実行すると、SQLite初期化、local server、child processを使う複数packageが同時に動作した。その結果、ローカルでは通る3秒／5秒の待機がhosted runnerで切れ、失敗するtestがrunごとに変動した。

Windows Gateではtestを省略せず、package schedulingだけを`go test -p 1 ./...`で直列化した。

### 1.4 Actions Summaryだけでは原因を確認できなかった

Actions Summaryのannotationには代表的なembed errorしか表示されなかったが、完全ログにはRecovery、Traffic fixture、launcher、server timeoutも含まれていた。Summaryまたはスクリーンショットだけで原因を一件と断定してはいけない。

## 2. workflowの必須contract

`.github/workflows/windows-gate.yml`のGo jobは、clean checkout単体で次の順序を完結させる。

1. repository checkout
2. Go setup
3. Node setup
4. `desktop`で`npm ci`
5. `desktop`で`npm run build:wails`
6. repository rootで`CGO_ENABLED=0 go test -p 1 ./...`

`npm run build:web`と`npm run build:wails`は同じではない。

- `build:web`：`desktop/dist`へ出力する。
- `build:wails`：Go embedが読む`desktop-app/frontend/dist`へ出力する。

Go jobが必要とする生成物は、必ずGo job自身で生成する。artifact方式へ変更する場合は、producerが`build:wails`を実行し、consumerがGo test前に同じrelative pathへdownloadすることをtestする。

## 3. Windows path安全規則

### 3.1 path文字列の直接比較をしない

Windowsでは次が同じ物理directoryを表す場合がある。

- 大文字と小文字の違い
- 8.3短縮名と長い名前
- 通常pathと許可されたverbatim表記

testや非セキュリティ用途の同一性確認では、Windows APIまたは物理解決結果を使う。`strings.EqualFold(filepath.Clean(a), filepath.Clean(b))`だけでは8.3表記を扱えない。

### 3.2 reparse安全性を弱めない

短縮path対応を理由に、次を許可してはならない。

- junction／symlinkを解決してから無条件に同一と判定する
- reparse attribute検査を削除する
- volume identity検査を削除する
- trusted base外のbackupを許可する

短縮表記の展開とredirect解決は別操作として扱う。短縮表記は正規化してよいが、redirectは引き続きfail-closedにする。

### 3.3 Windows test fixtureはtrusted baseを明示する

backupやRecoveryの実Windows APIを使うtestでは、real user profileへ依存しない。fixture rootを作成し、必要に応じて`LOCALAPPDATA`をそのrootへ設定し、backup directoryを配下へ置く。

「`LOCALAPPDATA`が空のときだけ設定する」fixtureは禁止する。hosted runnerでは値が設定済みでも、testの一時directoryが短縮表記になり、trusted-base比較が不安定になる。

## 4. push前の必須検証

Windows Gateを変更するcommit、Wails Desktopに影響するcommit、release commitでは、次をclean checkout相当の状態で一巡させる。

```powershell
npm --prefix desktop ci
npm --prefix desktop run build:wails
npm --prefix desktop run test:web
$env:CGO_ENABLED = "0"
go test -count=1 -p 1 ./...
git diff --check
git status --short
git diff --cached --name-only
```

生成済みの`desktop-app/frontend/dist`がローカルに残っているだけでは検証にならない。clean checkoutで再現する必要がある場合は、ユーザー所有のdirty worktreeをclean/resetせず、専用のclean worktreeまたはCIと同等の一時checkoutを使う。

`npm audit`警告は記録するが、依存更新を同じCI修正commitへ無断で混ぜない。dependency/security remediationは別scopeで行う。

## 5. GitHub Actions失敗時の診断手順

1. Summaryのannotationとjob名を確認する。
2. `gh run view <run-id> --log-failed`で完全ログを取得する。
3. 最初のerrorだけでなく、すべての`FAIL`、panic、timeout、失敗packageを列挙する。
4. frontend jobとGo jobのfilesystemは別物として扱う。
5. 同じcommitのfailed job再実行は、一過性判定のため最大1回を原則とする。
6. 再実行で失敗箇所が移動する場合は、資源競合、port、timeout、global environment、package並列性を疑う。
7. 原因を隠すためにtestをskipしたり、timeoutだけを無制限に延ばしたりしない。
8. localで対象testとWindows Gate相当の全体コマンドを通してからpushする。
9. push後は新runがgreenになるまで追跡し、push成功だけで完了としない。

## 6. commitとpushの規則

- commit前にstage対象pathを明示して確認する。
- author／committerはrelease方針で定めたidentityを使用する。
- `Co-Authored-By`、AI attribution、tool-call断片を自動追加しない。
- 通常の`git push origin main`を使う。
- CI修正のためにhistory rewriteやforce pushを行わない。
- hosted runnerでのみ追加問題が判明した場合は、既push commitを書き換えず、原因を記録した最小follow-up commitとする。

## 7. release順序

原則として次の順序を守る。

1. versionとREADMEを更新する。
2. frontend test、Wails embed build、serial Go test、正式Windows buildを通す。
3. release commitを通常pushする。
4. pushで起動した必須Actionsがすべてgreenであることを確認する。
5. tagとGitHub Releaseを作成する。
6. Setup.exeとchecksumをuploadする。
7. local／remote HEAD、tag、asset digest、release notesを確認する。

Actionsがgreenになる前にReleaseを公開しない。これにより「成果物は公開済みだがmainの必須checkは赤い」という状態を避ける。

既に公開したbinaryへ影響しないCI/test-only修正の場合、ReleaseやSetup.exeを作り直さない。製品コード、embedded frontend、version metadata、build toolchain、installer内容が変わった場合だけ、新しいbinary／release対応を判断する。

## 8. 完了条件

push／release作業は、次がすべて成立して初めて完了とする。

- local verificationがPASS
- `git diff --check`がPASS
- stageに意図したpathだけが存在
- author／committer／message／trailerが方針どおり
- normal push成功
- local HEADとremote mainが一致
- Windows Gateのfrontend jobがgreen
- Windows GateのGo jobがgreen
- worktreeとstageが想定どおり
- Release対象ならtag、Setup.exe、checksum、release notesを確認済み

「pushできた」「Setup.exeをuploadできた」だけでは完了ではない。必須Actionsのgreen確認までを一つのdelivery作業として扱う。
