# digitalx-cli

[English](README.md) · **한국어**

**[디지털엑스 개발자센터](https://developers.digitalx.miraeasset.com/)** · [API 문서](https://docs.digitalx.miraeasset.com/)

디지털엑스(Digital X) 암호화폐 거래소의 [Digital X Open API v2](https://docs.digitalx.miraeasset.com/)를 위한 커맨드라인 클라이언트입니다. 시세 조회, 거래, API 키 관리를 단일 실행 파일 하나로 제공합니다.

`digitalx-cli`는 **에이전트 우선(agent-first)**으로 설계되었습니다. AI 에이전트가 [Agent Skill 또는 MCP](#ai-에이전트에서-사용하기)를 통해 사용자를 대신해 디지털엑스와 안정적으로 연동할 수 있도록 만들어졌으며, 직접 셸에서 쓰기에도 그대로 편리합니다.

## digitalx-cli를 써야 하는 이유

- **단일 실행 파일.** 정적으로 빌드된 Go 바이너리 하나 — 별도 런타임이나 의존성이 없습니다.
- **기본이 안전.** 주문 수량, 소수점 문자열, enum 값, id 규칙을 전송 전에 검증하며, 주문 접수는 멱등적(idempotent)이라 재시도해도 주문이 두 번 들어가지 않습니다.
- **키 관리는 알아서.** 등록 링크로 온보딩하므로 어디에도 비밀 값을 붙여넣을 필요가 없습니다. 개인키는 로컬에서 생성되어 기기를 벗어나지 않으며, macOS 키체인 저장도 선택할 수 있습니다.
- **사람에게도 스크립트에도 친화적.** 모든 명령이 깔끔한 사람용 출력을 보여주고, `--json`(또는 `--compact`)을 붙이면 기계가 읽기 좋은 단일 문서를 냅니다. 종료 코드도 일관됩니다.

## 설치

아래 한 줄이면 플랫폼에 맞는 최신 릴리스를 내려받아 SHA-256을 검증하고 `dgx-cli`를 `PATH`에 추가합니다:

```sh
# Linux / macOS
curl -fsSL https://docs.digitalx.miraeasset.com/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://docs.digitalx.miraeasset.com/install.ps1 | iex
```

이후 바이너리는 스스로 관리됩니다 — `dgx-cli self update`, `dgx-cli self doctor`, `dgx-cli self uninstall`.

이전 릴리스에서 업그레이드하나요? [`MIGRATION.md`](MIGRATION.md)를 참고하세요.

직접 관리하고 싶다면 릴리스 바이너리를 내려받거나, 소스에서 `go install github.com/digitalx-official/digitalx-cli@latest`로 설치하세요.

## 빠른 시작

```sh
# 1. ED25519 키쌍을 생성하고 설정을 시작합니다. 개인키는 로컬 키스토어에 보관됩니다.
dgx-cli setup --name trading-bot

# 2. `setup`이 등록 링크를 출력합니다 — 링크를 열어 미리 채워진 공개키,
#    권한, IP 화이트리스트를 확인하고 MFA로 승인하세요. 그러면 setup이 새 키를 감지해
#    바인딩하고 상태 점검(health check)까지 자동으로 수행합니다.
```

이제 거래합니다:

```sh
dgx-cli ticker btc_krw
dgx-cli balance --currencies krw,btc
dgx-cli order place --symbol btc_krw --side buy --type limit --price 100000000 --qty 0.001
dgx-cli order get --symbol btc_krw --client-order-id <place 출력에 나온 id>
dgx-cli order cancel --symbol btc_krw --order-id 123456
```

## 할 수 있는 일

| 분야 | 명령 |
|---|---|
| 시세 (공개) | `ticker` `orderbook` `trades` `candles` `pairs` `ticksize` `currencies` `time` |
| 주문 & 체결 | `order place` `order get` `order cancel` `order open` `order history` `fills` |
| 어카운트 | `balance` `fees` `whoami` |
| 입출금 | `deposit …` `withdraw …` `krw deposit/withdraw …` |
| 실시간 | `tui` `monitor` |
| 키 & 설정 | `setup` `doctor` `ip` `key …` `keystore …` |
| 로컬 샌드박스 | `sandbox …` |
| 메타 | `commands` `logs` `license` `mcp serve` |

전체 목록은 `dgx-cli --help`, 개별 명령의 상세는 `dgx-cli <command> --help`로 확인하세요.

### 터미널 대시보드

`dgx-cli tui`는 전체 화면 대화형 대시보드를 엽니다 — 터미널에서 디지털엑스 시세를 가장 빠르게 지켜보는 방법입니다. 실시간 가격, 호가창, 최근 체결, 캔들 차트를 스트리밍하고, 둘러보는 대로 심볼을 전환합니다. 키로 로그인하면 잔고와 미체결 주문도 실시간으로 볼 수 있습니다. `--public`을 붙이면 API 키 없이 시세만 보는 화면이 됩니다.

```sh
dgx-cli tui                     # 잔고와 주문을 시세와 함께 표시
dgx-cli tui --public            # 시세만 — API 키 불필요
```

### 실시간 스트림

`dgx-cli monitor`는 WebSocket API를 한 줄에 JSON 하나씩 스트리밍합니다 — 공개 시세와, 키로 서명하면 본인의 주문·체결·잔고까지 받아볼 수 있습니다. 끊기면 자동으로 재연결하고 누락 구간을 보충하며, 내장 `--jq` 프로그램으로 출력을 필터링하거나 가공할 수 있고, WebSocket API 자체에 없는 실시간 OHLCV 캔들(`--candles`)을 합성할 수 있습니다.

```sh
dgx-cli monitor --symbols btc_krw --ticker --json
dgx-cli monitor --symbols btc_krw --candles 1,60 --candle-history 100 --json
dgx-cli monitor --symbols btc_krw --my-orders --my-assets --json
```

## AI 에이전트에서 사용하기

`digitalx-cli`는 사람뿐 아니라 AI 에이전트가 구동하도록 설계되었으며, 검증·기록·안전 규칙이 동일하게 적용되는 명령 표면을 두 가지 방식으로 제공합니다.

- **Agent Skill** — 리서치, 모니터링, 거래, 입출금, 설정, 디버깅 전반에서 에이전트가 CLI를 안전하게 다루도록 안내합니다. `dgx-cli agent skill install --all`로 설치하고 `dgx-cli agent skill doctor`로 확인하세요.
- **MCP 서버** — `dgx-cli mcp serve`는 모든 엔드포인트를 [Model Context Protocol](https://modelcontextprotocol.io) 도구로 노출하여 Claude, Codex 같은 호스트에서 쓸 수 있게 합니다. 시세 조회만 허용하려면 `--read-only`를 붙이세요.

```sh
# MCP 서버 등록 (서버당 키 하나)
claude mcp add --transport stdio digitalx-cli -- dgx-cli mcp serve --key trading
codex  mcp add digitalx-cli -- dgx-cli mcp serve --key trading
```

터미널 없이 쓰는 Claude Desktop이라면, 릴리스 페이지에서 드래그 앤 드롭용 [`.mcpb` 번들](https://github.com/anthropics/mcpb)로 서버를 설치할 수 있습니다 — 바이너리가 함께 포함되어 있습니다.

## 실제 자금 없이 테스트하기

- **드라이런(dry-run)** — 어떤 명령이든 `--dry-run`을 붙이면 실제로 전송하지 않고 전송될 요청을 그대로 출력합니다. `order place`에서는 위험한 주문(슬리피지, 잘못 입력한 가격, 체결 불가 수량 등)을 경고하는 고객 보호 사전 점검도 함께 실행합니다.
- **로컬 샌드박스** — `dgx-cli sandbox start`는 실제 서명 검증기를 갖춘 단일 파일 로컬 목(mock) API를 실행합니다. 운영 환경이나 실제 자금을 건드리지 않고 서명과 전체 주문 흐름을 시험해 볼 수 있습니다:

```sh
dgx-cli sandbox start          # 샌드박스 내려받아 실행하고, 시드 키 가져오기
dgx-cli whoami --key sandbox    # 이제 서명된 호출이 로컬 목으로 전달됨
dgx-cli sandbox stop
```

`sandbox start`에 `--paper --fresh`를 붙이면 **페이퍼 트레이딩** 모드입니다: 시장 데이터는 실제 디지털엑스 프로덕션에서 실시간으로 미러링되고 체결은 시뮬레이션으로 유지됩니다 — 실제 가격, 실제 자금 없음. 기본적으로는 번들에 내장된 픽스처 거래쌍만 시드됩니다. `--all-pairs`를 추가하면(`dgx-cli sandbox start --paper --all-pairs --fresh`) 라이브 스냅샷에서 **launched 상태의 모든 프로덕션 거래쌍**을 시드하여 샌드박스가 프로덕션의 거래 가능한 거래쌍 집합을 갖게 됩니다. 이런 첫 기동은 몇 초가 더 걸리지만, 이후 스냅샷은 데이터베이스 옆에 캐시되므로 반복 기동(`--fresh` 포함)은 1초 이내에 재시드됩니다. `--fresh`는 일회용 샌드박스 데이터베이스를 새로 만들며, 원래 모드로 돌아갈 때도 같은 방법을 씁니다. 기존 페이퍼 샌드박스를 잔고와 주문을 유지한 채 **재시작**하려면 `--fresh` 없이 `dgx-cli sandbox start --paper`를 실행하세요 (`dgx-cli sandbox start --help`에 상세 설명).

샌드박스 번들은 별도의 라이선스가 적용됩니다(디지털엑스 자체 소프트웨어) — 약관은 `dgx-cli sandbox license`로 확인하세요. 로컬 개발 및 테스트 용도로만 사용하세요.

## 안전성

- **멱등적 주문 접수.** 모든 `order place`에는 `clientOrderId`가 실립니다(자동 생성되어 출력에 함께 표시). `--client-order-id`로 같은 id를 재사용하면 *동일한* 주문을 재시도하는 것이며, 디지털엑스는 이를 한 번만 처리하고 CLI는 무작정 다시 보내지 않고 대사(reconcile)합니다.
- **클라이언트 측 수량 지정.** `limit` → `--price` + `--qty`; 시장가/best **매수** → `--amt`(사용할 KRW); 시장가/best **매도** → `--qty`. 금액은 소수점 문자열 그대로 전달되며 — 부동소수점 변환이 없습니다.
- **자금 이중 전송 방지.** 조회와 취소는 일시적 오류 시 자동 재시도하지만, 자금이 움직이는 쓰기(`order place`, 출금, KRW 이체)는 절대 자동 재시도하지 않습니다.

## 키 & 저장소

키를 여러 개 추가할 수 있으며, 명령마다 `--key`로 하나를 선택합니다(기본값은 `dgx-cli key use`로 지정). ED25519와 HMAC-SHA256 키를 모두 지원합니다. 개인키는 기본적으로 디스크에 암호화되어 저장되거나 OS 키체인에 보관되며, 모든 상태는 `~/.digitalx-cli/` 아래에 있습니다. 키는 `dgx-cli key …`로 관리하고, 백엔드 간 이동은 `dgx-cli keystore migrate`로 합니다.

## 라이선스

Copyright © 2026 Digital X Co., Ltd.

Apache License, Version 2.0(`SPDX-License-Identifier: Apache-2.0`)에 따라 배포됩니다. 전문은 [`LICENSE`](LICENSE)를 참고하세요.

**면책 조항** — 본 도구를 사용하기 전에 [`DISCLAIMER.ko.md`](DISCLAIMER.ko.md)를 읽어주세요.

로컬 샌드박스 번들(`dgx-cli sandbox …`)은 이 라이선스의 적용을 받지 **않습니다** — 디지털엑스 주식회사의 독점 소프트웨어로 별도의 약관이 적용됩니다. CLI는 공식 출처(Official Source)에서만 내려받아 실행하며, 약관은 `dgx-cli sandbox license`로 확인할 수 있습니다.
