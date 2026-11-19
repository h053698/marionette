# 기기 배정, 네이티브 자동화, 원격 ADB와 작업 큐

프론트엔드, REST, Go SDK, CLI, MCP와 작업 러너는 같은 독점 점유를 사용합니다. 한 계정은 모든 에이전트를 통틀어 동시에 한 기기만 점유할 수 있습니다. 중계 백엔드는 조건에 맞는 기기를 선택하고, 해당 에이전트가 점유를 원자적으로 발급합니다. 일반 사용자의 기기 목록에는 본인에게 배정된 기기만 표시하며, 배정 전에는 빈 목록입니다. 관리자만 전체 기기를 모니터링하고 기존 점유를 종료하거나 가져올 수 있습니다.

## CLI로 기기 배정받기

```sh
make cli mcp
export PATH="$PWD/build:$PATH"
export MARIONETTE_URL=http://127.0.0.1:8080
read -r MARIONETTE_TOKEN  # 웹 콘솔 API / MCP에서 발급한 개인 토큰
export MARIONETTE_TOKEN

marionette devices
marionette occupy --platform android --tags lab --ttl 300
marionette info
marionette tree --format xml
marionette screenshot -o screen.png
marionette release
```

`--tags`는 쉼표로 구분하며 모든 태그를 만족해야 합니다. 에이전트의 `--tags` 또는 `MARIONETTE_DEVICE_TAGS`가 해당 PC의 기기에 공통 적용됩니다. 기기별 태그 관리 UI는 없습니다. `--model`은 목록의 모델 문자열과 정확하게 비교하고, `--device`는 목록의 전역 ID를 지정합니다. 일치하는 사용 가능 기기가 없으면 즉시 409를 반환합니다. 작업 큐는 같은 상황에서 기기를 기다립니다.

CLI는 기본 `.marionette/session.json`에 배정 정보와 점유 토큰을 0600 권한으로 저장합니다. 개인 API 토큰은 저장하지 않습니다. 세션 파일이나 개인 토큰을 여러 개 만들어도 계정당 한 기기 제한은 같습니다. 다른 기기를 배정받으려면 현재 기기를 먼저 반납합니다. `renew --ttl 300`으로 갱신하거나 `release`로 반납합니다. 일반 CLI 명령은 자동 갱신하지 않습니다.

`marionette release --wipe`는 반납과 함께 설치한 앱과 내부 저장소(`/sdcard`) 파일 정리를 요청합니다. 정리가 끝나면 다음 사용자에게 배정할 수 있습니다. 기기 계정 로그인 같은 시스템 설정은 남을 수 있습니다.

반납에는 저장된 점유 토큰을 사용합니다. 다른 점유 명령은 서버에서 같은 계정·기기·점유 ID를 확인해 저장된 만료 시간을 최신 상태로 바꿉니다. 브라우저에서 갱신한 점유도 계속 사용할 수 있으며, 만료·반납·교체된 점유는 자동으로 다시 배정하지 않습니다. 같은 조건으로 `occupy`를 다시 실행하면 저장된 유효 점유를 이어서 사용합니다.

다른 디렉터리에서도 `marionette`를 실행하려면 빌드한 CLI를 PATH 안에 설치하세요. 예를 들어 `mkdir -p "$HOME/.local/bin"` 후 `ln -s "$PWD/build/marionette" "$HOME/.local/bin/marionette"`로 연결하고, 셸의 PATH에 `$HOME/.local/bin`을 포함합니다. ADB도 `adb version`으로 실행되는지 확인하세요. CLI 실행 파일 설치와 `MARIONETTE_TOKEN` 설정은 각각 필요합니다. 브라우저의 연결 코드는 90초 뒤 만료되므로 설치가 끝난 뒤 새 코드를 만드세요.

기기를 점유한 상태에서 `marionette apps`는 설치된 실행 가능 앱을, `marionette processes`는 기기의 앱·시스템 프로세스 목록을 조회합니다. 프로세스 결과에는 PID, PPID, UID, 상태와 RSS(KB)가 포함됩니다. Android shell UID가 조회할 수 있는 범위이며, 프로세스 종료 명령은 제공하지 않습니다. 브라우저의 **앱 · 프로세스**에서 실제 앱 아이콘을 확인하고, **실행 중** 탭에서 검색하거나 5초 간격으로 목록을 갱신할 수 있습니다.

## ID로 찾기, 클릭과 Unicode 입력

기기 안에서 실행 중인 Java 드라이버의 UiAutomation을 사용합니다. 요청마다 `adb shell uiautomator dump`를 실행하지 않습니다.

```sh
marionette find --id com.example.app:id/search --wait 5000
marionette click --id com.example.app:id/search
marionette type --id com.example.app:id/search --value '안녕하세요 🙂'
marionette find --text '확인' --class android.widget.Button
marionette tap '{"x":360,"y":600}'
marionette swipe '{"x1":360,"y1":1000,"x2":360,"y2":300,"durationMs":400}'
marionette key '{"keyCode":3}'
```

선택자는 `resourceId`, `text`, `description`, `className`, `index`를 조합합니다. 문자열은 정확하게 일치해야 하며 `index`는 0부터 시작합니다. `waitMs`는 0–8000ms입니다. CLI의 선택자 명령은 기본 5000ms 기다립니다.

텍스트는 입력란의 기존 내용을 교체합니다. 접근성으로 편집할 수 없는 입력란은 실패합니다. 탭·스와이프와 요소 `bounds`는 PNG 캡처의 물리 픽셀 좌표입니다. 트리는 최대 2000개 노드, 깊이 64로 제한합니다. 동작 응답 직후 앱의 화면 전환이 끝났다고 가정하지 말고 다음 요소나 캡처로 확인하세요.

## REST와 Go SDK

`POST /api/device/occupy`에 다음 본문을 보내면 배정과 드라이버 시작을 한 번에 처리합니다. `POST /api/devices/occupy`도 같은 API입니다.

```json
{"platform":"android","tags":["lab"],"ttlSeconds":300}
```

응답은 `{assigned,device,lease}`이며 `lease`에 `id`, `token`, `owner`, `expiresAt`이 있습니다. 후속 조작에는 개인 토큰의 `Authorization: Bearer ...`와 점유 토큰의 `X-Lease-Token`을 모두 보냅니다.

`GET /api/pool`의 `virtual.devices` 또는 `physical.devices`에 있는 `id`를 `deviceId`로 보내면 해당 기기를 요청할 수 있습니다. 이 값은 발급된 계정에서 최대 10분간 유효합니다. 만료됐거나 백엔드가 재시작됐다면 목록을 다시 조회하세요. 관리자 세션과 CLI처럼 개인 토큰으로 인증하는 클라이언트는 기존 기기 ID도 사용할 수 있습니다. 배정받지 않은 기기에 대한 회원의 `/api/actions/{action}` 요청은 `403`과 `device not available`을 반환합니다.

| 경로 | 본문 / 결과 |
| --- | --- |
| `POST /api/devices/{id}/actions` | `{action:"click",selector:{resourceId:"..."},waitMs:5000}` |
| 같은 경로 | `{action:"findElement",selector:{text:"확인"}}` |
| 같은 경로 | `{action:"text",selector:{resourceId:"..."},text:"한글🙂"}` |
| 같은 경로 | `{action:"uiTree",format:"xml"}` 또는 `format:"json"` |
| `GET /api/devices/{id}/screenshot` | PNG |
| `POST /api/actions/click` | `{deviceId,selector,waitMs}`; 선택자가 없으면 `{deviceId,x,y}` |
| `POST /api/actions/input` | `{deviceId,text,selector}` |
| `POST /api/actions/dump` | `{deviceId,format}` |

Go에서는 `client.New`, `Occupy`, `FindElement`, `Click`, `SetText`, `UITree`, `Screenshot`, `Renew`, `Release`를 사용합니다. 모든 호출은 `context.Context`를 받습니다. `Occupy`의 조건 타입은 `client.Selection`, 요소 조건은 `client.Selector`입니다. 기존 MCP 도구도 조건 배정과 선택자를 지원합니다. [API / MCP 사용법](API_MCP.md)을 참고하세요.

## 배정된 Android를 내 PC의 ADB와 Android Studio에 연결

사용자 PC에는 Android SDK Platform Tools의 `adb`가 필요합니다. Android Studio의 SDK에 포함된 실행 파일도 사용할 수 있습니다.

```sh
marionette occupy --platform android --ttl 300
marionette adb --adb /absolute/path/to/platform-tools/adb
```

명령이 `127.0.0.1:<포트>`를 출력하고 로컬 ADB 서버에 연결합니다. Android Studio의 기기 선택 목록에서 해당 기기를 선택하세요. 같은 SDK의 ADB를 사용해야 같은 서버 목록을 볼 수 있습니다. 포트를 고정하려면 `--port 15555`를 지정합니다.

```sh
adb -s 127.0.0.1:15555 shell
adb -s 127.0.0.1:15555 install app-debug.apk
adb -s 127.0.0.1:15555 logcat
adb -s 127.0.0.1:15555 push local.txt /data/local/tmp/local.txt
```

`marionette adb`를 켜 둔 동안 점유를 갱신합니다. Ctrl+C는 중계를 끊고 자동 갱신을 멈춥니다. `marionette release`는 점유를 바로 반납합니다. 관리자 연결 종료, 점유 만료 또는 토큰 폐기 때 기존 중계도 종료합니다. 점유한 기기의 ADB 서비스만 인증된 WebSocket으로 전달하며, 호스트의 5037 포트를 외부에 공개하거나 폰을 TCP 5555 모드로 바꾸지 않습니다.

ADB 중계와 직접 연결은 점유할 때의 ADB 전송 인스턴스에 묶입니다. 같은 시리얼로 다시 연결되더라도 전송 ID가 바뀌면 에이전트가 기존 점유와 ADB 연결을 종료합니다. 작업을 이어가려면 다시 점유하고 `marionette adb`를 실행하세요.

### 직접 연결 (WebRTC)

`marionette adb`는 기본값(`--mode auto`)으로 에이전트와 WebRTC 직접 연결을 먼저 시도합니다. 백엔드는 점유 토큰으로 인증한 연결 제안(offer/answer)만 전달하고, ADB 데이터는 사용자 PC와 에이전트 PC 사이를 직접 오갑니다. 에이전트는 들어오는 포트를 열지 않습니다. 양쪽이 먼저 바깥으로 연결을 걸어 NAT를 통과하며, 실패하면 기존 백엔드 중계로 자동 전환합니다. 출력의 `Connection path:` 줄에서 실제 경로를 확인할 수 있습니다.

| 옵션 | 동작 |
| --- | --- |
| `--mode auto` | 직접 연결을 시도하고, 실패하면 백엔드 중계를 씁니다(기본값). |
| `--mode direct` | 직접 연결만 씁니다. 실패하면 종료합니다. |
| `--mode relay` | 항상 백엔드 중계를 씁니다. |
| `--ice URL,...` | STUN/TURN 서버 목록입니다. 기본값은 공개 STUN(`stun:stun.l.google.com:19302`)이고, `none`은 같은 망 안의 주소만 씁니다. 환경 변수 `MARIONETTE_ICE_SERVERS`로도 지정합니다. |

에이전트도 같은 `--ice` 옵션과 `MARIONETTE_ICE_SERVERS`를 지원합니다. 회사망처럼 UDP가 막힌 환경에서는 직접 연결이 실패할 수 있습니다. 이때는 자동으로 백엔드 중계를 쓰거나, 양쪽에 같은 TURN 서버를 지정합니다. 직접 연결도 중계와 같은 규칙을 따릅니다. 점유한 기기의 서비스만 열 수 있고, `reverse`는 차단하며, 점유가 끝나면 에이전트가 연결을 직접 끊습니다.

직접 연결 한 개에서 열 수 있는 데이터 채널은 누적 64개입니다. 한도에 도달해 연결이 끊기면 `marionette adb`를 다시 실행하거나 여러 ADB 서비스 연결이 필요한 작업에는 `--mode relay`를 사용하세요.

`adb forward`와 JDWP는 사용자 PC의 로컬 ADB 서버를 통해 사용합니다. `adb reverse`는 지원하지 않습니다. 현재 중계에서 reverse를 전달하면 목적지가 사용자 PC가 아닌 에이전트 PC가 되므로, 에이전트 내부 포트·소켓에 접근하지 못하도록 차단합니다. Android Studio의 설치·실행·JDWP와 별개로, Metro 같은 개발 서버의 reverse 연결이 필요한 도구에는 추가 연결 구성이 필요합니다.

대시보드에서 이미 기기를 선택했다면 **점유하고 조작 → Android Studio · ADB → 연결 명령 만들기**를 사용합니다. 표시된 `marionette attach --device ... --code ...`와 `marionette adb`를 터미널에서 실행합니다. 같은 OIDC 계정에서 발급한 개인 토큰이 필요하며, 코드는 90초 동안 한 번만 유효합니다. CLI가 코드를 받으면 점유 토큰을 교체하고 브라우저의 조작 연결을 종료합니다. 화면에 남은 이전 토큰으로 새 CLI 점유를 반납할 수 없습니다.

## Kafka 작업 큐와 여러 러너

Kafka 설정이 없으면 수동 배정·MCP·ADB는 계속 동작하고 작업 API만 503을 반환합니다. 큐를 사용할 때 백엔드와 러너에 다음 값을 설정합니다.

```sh
export MARIONETTE_KAFKA_BROKERS=127.0.0.1:9092
export MARIONETTE_KAFKA_TOPIC=marionette.jobs
read -r MARIONETTE_RUNNER_TOKEN  # 32바이트 이상의 별도 랜덤 토큰
export MARIONETTE_RUNNER_TOKEN
```

Kafka 토픽을 미리 만들고 병렬 처리 수 이상의 파티션을 설정합니다. 로컬 KRaft 설정 예시는 [kafka-local.properties](../deploy/kafka-local.properties)입니다. 외부 브로커는 `MARIONETTE_KAFKA_TLS=true`를 사용하고, SASL/PLAIN이 필요하면 `MARIONETTE_KAFKA_USERNAME`, `MARIONETTE_KAFKA_PASSWORD`를 함께 설정합니다. SASL/PLAIN에는 TLS가 필수입니다.

```sh
make backend runner cli
# 위 환경 변수와 기존 OIDC·에이전트 설정을 적용한 뒤 백엔드 실행
build/marionette-backend --job-file /persistent/private/jobs.json

# 서로 다른 터미널 또는 서버에서 같은 group으로 여러 개 실행
build/marionette-runner --backend https://api.example.com --group marionette-runners
```

러너 하나는 한 번에 한 작업을 실행합니다. 같은 소비자 그룹의 여러 러너가 파티션을 분담합니다. Kafka에는 불투명한 작업 ID만 넣고, 동작 본문·사용자 정보·점유 정보는 백엔드의 비공개 저장소에 둡니다. 백엔드는 별도의 작업 claim을 발급하고, 단계 실행 전에 상태를 영속화해 중복 배달이 같은 조작을 재실행하지 않게 합니다.

```sh
marionette job submit --file examples/android-job.json
marionette job wait --id <반환된_작업_ID>
marionette job artifact --id <작업_ID> --index 2 -o result.png
marionette job status --id <작업_ID>
marionette job cancel --id <작업_ID>
```

REST는 `POST /api/jobs`, `GET /api/jobs/{id}`, `DELETE /api/jobs/{id}`, `GET /api/jobs/{id}/artifacts/{index}`입니다. 조회·취소·삭제·결과 접근은 작업 소유자와 관리자에게만 허용합니다. `DELETE`는 대기 중이거나 진행 중인 작업을 취소하고 `200`과 취소된 기록을 반환합니다. 이미 끝난 작업에 `DELETE`를 보내면 기록과 결과 파일을 삭제하고 `204`를 반환합니다. `client.SubmitJob`, `Job`, `CancelJob`, `JobArtifact`도 제공합니다. `CancelJob`과 CLI의 `job cancel`에도 같은 삭제 규칙이 적용됩니다.

작업은 1–100단계, 제한 시간 10–3300초이며 기본 300초입니다. 점유 최대 시간이 더 짧으면 제한 시간 상한은 점유 최대 시간에서 30초를 뺀 값입니다. 러너에 보낼 claim 응답이 900 KiB를 넘는 요청은 `413`과 `job_payload_too_large`를 반환합니다. 이 경우 단계 수나 입력 크기를 줄여서 제출하세요. 기기가 모두 점유 중이면 제한 시간 안에서 기다립니다. 완료·실패·취소 시 기기 반납을 시도합니다. heartbeat가 45초 이상 끊기거나 백엔드가 재시작되면 진행 중 작업을 실패로 종료합니다. 이미 실행됐을 수 있는 조작을 자동 재생하지 않으므로 사용자가 새 작업을 명시적으로 제출해야 합니다. 실패·취소 기록에는 안정적인 `errorCode`와 한국어 `error`가 담깁니다. 운영자는 백엔드 로그에서 작업 ID로 상세 오류를 찾을 수 있습니다.

현재 영속 저장은 **단일 백엔드가 소유하는 JSON 파일**입니다. 에이전트와 러너는 여러 프로세스로 운영할 수 있지만, 여러 백엔드가 같은 JSON 파일을 쓰는 구성이나 장애 시 다른 백엔드로 잠금을 이전하는 분산 DB 구성은 지원하지 않습니다. 사용자별 활성 작업은 최대 20개이며 전체 상한은 기록 4096개와 저장 파일 128 MiB입니다. 백엔드는 끝난 작업과 결과 파일을 기본 168시간 후 자동으로 정리합니다. 저장 공간이나 계정별 결과 파일 한도에 가까워지면 오래된 종료 작업을 먼저 정리합니다. 결과 파일의 기본 한도는 계정당 256 MiB입니다. 기간과 한도는 [환경 변수 설정](CONFIG.md)에서 조정할 수 있습니다. 보관이 필요한 결과는 삭제되기 전에 내려받으세요. 실패나 취소 전에 실행된 기기 동작은 그대로 남습니다.

## 확인한 범위

2026-11-17에 Galaxy S10e / Android 12에서 네이티브 ID 탐색·클릭, 한글·이모지 전체 입력, UI XML, 캡처를 확인했습니다. 분리된 백엔드·에이전트를 거친 로컬 ADB 연결에서 셸, PNG 캡처, 파일 push/pull의 해시 일치와 반납 후 연결 종료를 확인했습니다. Android Studio 자체의 실행·디버깅 UI와 APK 설치·JDWP는 이번 검증에 포함하지 않았습니다.

실제 Kafka와 러너를 통해 Android 실기기와 에뮬레이터에서 작업의 HOME → UI XML → 캡처가 각각 완료됐습니다. 두 러너가 서로 다른 작업을 동시에 claim한 뒤, 점유 중인 에뮬레이터의 반환을 기다리고 같은 기기를 차례로 사용해 두 작업 모두 완료하는 흐름도 확인했습니다. 결과 PNG 다운로드와 일회용 연결 코드로 점유를 이동한 뒤의 이전 토큰·코드 재사용 차단을 확인했습니다.

Nebula 소개의 성능 표는 이 프로젝트의 측정 결과가 아니며 동일한 지연 시간이나 배수를 주장하지 않습니다.
