# korbit-cli has moved to digitalx-cli

**English** · [한국어](README.ko.md)

Korbit is now **Digital X** — see the [official announcement](https://digitalx.miraeasset.com/notice/detail/?noticeId=5GYMLMEeJs00JQLxuMQc2N) — and this CLI moved with it:

### → **https://github.com/digitalx-official/digitalx-cli**

Go there for the source, the latest release, and the documentation. This repository is no longer developed.

## Why this repository still exists

It serves the release archives that `korbit` binaries already installed on users' machines download when they update themselves. Those binaries ask for an archive by a name fixed when they were built, so the names have to keep resolving here. Nothing new is published in this repository.

## If you have `korbit` installed

Run `self update` **twice**:

```sh
korbit self update    # 1. installs the current release, still under the `korbit` name
korbit self update    # 2. adds the `digitalx` command, pointing `korbit` at it
```

The first run leaves you current but still `korbit`-only, because the code that creates the `digitalx` command ships inside the release it downloads. The second run is that new code.

**Nothing on your disk is moved or renamed.** Your home directory, your API keys and your databases stay where they are and keep working, and `korbit` keeps working as an alias of the same binary. See [MIGRATION.md](https://github.com/digitalx-official/digitalx-cli/blob/master/MIGRATION.md) for the full detail.

## If you are installing for the first time

```sh
curl -fsSL https://docs.digitalx.miraeasset.com/install.sh | sh
```

```powershell
irm https://docs.digitalx.miraeasset.com/install.ps1 | iex
```

---

# korbit-cli는 digitalx-cli로 이동했습니다

코빗이 **디지털엑스(Digital X)** 로 바뀌었습니다([공식 공지](https://digitalx.miraeasset.com/notice/detail/?noticeId=5GYMLMEeJs00JQLxuMQc2N)). 이 CLI도 함께 이동했습니다.

### → **https://github.com/digitalx-official/digitalx-cli**

소스 코드, 최신 릴리스, 문서는 모두 위 저장소에 있습니다. 이 저장소에서는 더 이상 개발이 진행되지 않습니다.

## 이 저장소가 남아 있는 이유

이미 사용자 컴퓨터에 설치된 `korbit` 실행 파일이 스스로 업데이트할 때 내려받는 릴리스 아카이브를 제공하기 위해서입니다. 해당 실행 파일은 빌드 시점에 정해진 이름으로 아카이브를 요청하므로, 그 이름이 이곳에서 계속 응답해야 합니다. 이 저장소에 새로 게시되는 것은 없습니다.

## `korbit`이 설치되어 있다면

`self update`를 **두 번** 실행하세요.

```sh
korbit self update    # 1. 현재 릴리스를 설치합니다. 이름은 아직 `korbit`입니다
korbit self update    # 2. `digitalx` 명령을 추가하고 `korbit`이 이를 가리키게 합니다
```

`digitalx` 명령을 만드는 코드가 첫 번째 실행에서 내려받는 릴리스 안에 들어 있습니다. 그래서 첫 실행은 최신 버전으로 만들어 줄 뿐 이름은 여전히 `korbit`뿐이고, 두 번째 실행이 바로 그 새 코드입니다.

**디스크에 있는 것은 아무것도 옮겨지거나 이름이 바뀌지 않습니다.** 홈 디렉터리, API 키, 데이터베이스는 있던 자리에서 그대로 동작하며, `korbit`도 같은 실행 파일의 별칭으로 계속 사용할 수 있습니다. 자세한 내용은 [MIGRATION.ko.md](https://github.com/digitalx-official/digitalx-cli/blob/master/MIGRATION.ko.md)를 참고하세요.

## 처음 설치하는 경우

```sh
curl -fsSL https://docs.digitalx.miraeasset.com/install.sh | sh
```

```powershell
irm https://docs.digitalx.miraeasset.com/install.ps1 | iex
```
