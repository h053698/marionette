# API / MCP 사용법

브라우저, REST 클라이언트, MCP는 같은 독점 점유를 공유합니다. 계정당 동시에 한 기기만 점유할 수 있습니다. 일반 사용자의 목록에는 본인에게 배정된 기기만 표시합니다. 배정 전에는 `list_devices`가 빈 목록을 반환하므로 `acquire_device`에 플랫폼·태그 조건을 주거나 인자 없이 기기를 배정받습니다. 다른 기기의 화면, 앱, 프로세스와 조작 API에는 접근할 수 없습니다. 관리자는 전체 기기 모니터링 권한을 유지합니다.

## 개인 토큰 발급

1. 웹 콘솔에서 회사 계정으로 로그인합니다.
2. 오른쪽 위 **API / MCP**를 열고 토큰 이름과 유효기간을 선택합니다.
3. **토큰 발급**을 누르고 토큰 또는 MCP 설정을 복사합니다. 원문은 이때 한 번만 표시됩니다.
4. 사용을 중단할 때 같은 창의 **내 토큰 → 폐기 → 폐기 확인**을 누릅니다.

토큰은 기본 30일, API로는 1–90일 유효기간을 지정할 수 있습니다. 한 사용자당 활성 토큰은 최대 20개입니다. 토큰을 폐기하면 해당 토큰의 진행 중 요청도 취소합니다. 기기 점유 자체는 반납하거나 TTL이 끝날 때 해제됩니다.

개인 토큰은 일반 사용자 권한입니다. 관리자 미리보기, 다른 사용자의 점유 탈취, 토큰 추가 발급에는 사용할 수 없습니다. 브라우저 로그아웃과 개인 토큰의 수명은 별개입니다. 토큰 원문을 로그나 저장소에 넣지 마세요.

## MCP 연결

### 원격 MCP (OAuth, 토큰 없이)

중계 백엔드가 `<백엔드 주소>/mcp`에서 Streamable HTTP MCP 서버를 제공합니다. Claude 같은 MCP 클라이언트의 커넥터(원격 MCP 서버) 추가 화면에 이 주소를 넣으면 클라이언트가 OAuth 2.1로 연결합니다.

1. 클라이언트가 `/.well-known/oauth-protected-resource`와 `/.well-known/oauth-authorization-server`를 읽고 `/oauth/register`로 스스로 등록합니다(공개 클라이언트, PKCE S256 필수).
2. 브라우저가 `/oauth/authorize`로 이동합니다. 로그인되어 있지 않으면 회사 계정으로 로그인한 뒤 동의 화면이 나오고, **허용**을 누르면 단회용 코드가 클라이언트로 전달됩니다.
3. 클라이언트가 `/oauth/token`에서 코드와 PKCE 검증값을 보내 액세스 토큰(기본 1시간)과 갱신 토큰(기본 30일)을 받습니다. 갱신할 때마다 토큰이 교체됩니다. 교체 직전 토큰을 10분 안에 다시 보내면 해당 연결 전체가 폐기됩니다.

OAuth 연결은 항상 일반 사용자 권한이며, 관리자 계정으로 허용해도 마찬가지입니다. 계정당 동시에 한 기기만 점유하는 정책과 본인에게 배정된 기기만 보이는 규칙이 그대로 적용됩니다. 점유 토큰은 서버가 사용자별로 보관하고 모델에는 노출하지 않습니다. 같은 계정으로 `acquire_device`를 다시 호출하면 이미 가진 점유를 돌려줍니다.

웹 콘솔 **API / MCP → MCP**의 **연결된 앱**에서 허용한 앱을 확인하고 **연결 끊기**로 폐기합니다. 폐기하면 해당 연결의 MCP 세션이 닫히고 진행 중인 도구 호출이 취소됩니다. 액세스 토큰이 만료되면 해당 토큰으로 실행한 호출이 취소되며, 갱신한 액세스 토큰으로 같은 세션을 이어갈 수 있습니다. 세션은 유휴 상태로 30분이 지나면 닫힙니다. 사용자당 최대 8개, 서버 전체에서 최대 1,024개의 세션을 열 수 있습니다.

등록 정보와 갱신 토큰의 해시는 `--oauth-file`(기본 `.marionette/oauth.json`, 0600)에 저장합니다. 액세스 토큰은 메모리에만 있어 백엔드를 재시작하면 클라이언트가 갱신 토큰으로 새로 받습니다. `/oauth/register`는 백엔드에 직접 접속하는 IP당 한 번에 8건까지 허용하고 30초마다 1건씩 한도를 채웁니다. 여러 클라이언트가 같은 리버스 프록시를 거치면 이 한도를 공유합니다.

로그인 요청도 직접 접속 IP당 한 번에 12건까지 허용하며 15초마다 1건씩 한도를 채웁니다. 같은 브라우저에서 로그인을 다시 시작하면 이전 로그인 요청을 대체합니다. 대기열이 가득 차면 새 로그인 요청을 잠시 거절합니다.

`MARIONETTE_PUBLIC_URL`이 OAuth 발급자 주소가 됩니다. 공개 주소에는 HTTPS가 필요하며, loopback 주소는 HTTP도 허용합니다. 리버스 프록시는 공개 주소와 일치하는 `Host`(포트가 있으면 포트 포함)를 백엔드에 전달해야 합니다. 브라우저가 `Origin`을 보내면 공개 주소 또는 설정된 프런트엔드 오리진과 일치해야 합니다.

### 로컬 stdio MCP (개인 토큰)

원격 연결을 쓸 수 없을 때는 사용자 PC에서 `marionette-mcp`를 직접 실행하는 방식을 씁니다. 중계 백엔드와 PC 에이전트가 실행 중인 상태에서 MCP 실행 파일을 빌드합니다. 이 실행 파일은 사용자 PC에서 중계 백엔드의 REST API로 접속하므로 사용자 PC에 ADB나 Android SDK를 설치할 필요가 없습니다.

```sh
make mcp
```

stdio MCP 클라이언트에 다음 서버 설정을 추가합니다. 클라이언트마다 설정 파일 위치와 등록 UI는 다를 수 있습니다. 웹 콘솔의 **설정 복사**는 현재 API 주소와 새 토큰을 채워 줍니다.

```json
{
  "mcpServers": {
    "marionette": {
      "command": "/absolute/path/to/marionette/build/marionette-mcp",
      "env": {
        "MARIONETTE_URL": "http://127.0.0.1:8080",
        "MARIONETTE_TOKEN": "mnt_발급한_토큰"
      }
    }
  }
}
```

`MARIONETTE_URL`은 백엔드의 오리진(`scheme://host[:port]`)입니다. 프런트엔드를 별도 배포했다면 백엔드 주소를 사용합니다. loopback 이외에는 HTTPS가 필요합니다. `--url` 플래그로도 주소를 지정할 수 있습니다. MCP 출력은 stdout, 로그는 stderr를 사용합니다.

에이전트에게 “사용 가능한 기기를 점유하고 화면을 확인한 뒤 설정 앱을 열어 줘. 끝나면 반납해 줘.”처럼 요청할 수 있습니다.

| 도구 | 입력 예시 | 동작 |
| --- | --- | --- |
| `list_devices` | `{}` | 내게 배정된 기기의 모델·시리얼·연결 상태·만료 시간 조회 |
| `acquire_device` | `{"platform":"android","tags":["lab"],"ttlSeconds":300}` | 조건에 맞는 기기를 배정하고 드라이버 시작. `serial`로 특정 기기도 지정 가능 |
| `renew_device` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","ttlSeconds":300}` | 이 MCP 프로세스의 점유 갱신 |
| `release_device` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","wipe":true}` | 기기 반납. 선택 항목 `wipe:true`는 설치한 앱과 내부 저장소 파일 정리를 요청합니다. 기본값은 `false`입니다. |
| `device_info` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA"}` | 디스플레이 정보. `format:"xml"`로 UI 트리, `selector`로 요소 조회, `apps:true`로 앱 목록, `processes:true`로 실행 중인 앱·시스템 프로세스(PID·UID·상태·RSS) 조회 |
| `screenshot` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA"}` | 물리 해상도의 PNG 이미지 반환 |
| `tap` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","selector":{"resourceId":"com.example:id/search"},"waitMs":5000}` | 선택자로 클릭하거나 `x`, `y`로 좌표 탭. `packageName:"com.android.settings"`로 앱 실행 |
| `swipe` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","x1":360,"y1":1000,"x2":360,"y2":300,"durationMs":400}` | 두 지점 사이 스와이프 |
| `press_key` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","key":"HOME"}` | Android 키 전송 |
| `type_text` | `{"serial":"office-mac.ZW11bGF0b3ItNTU4MA","text":"안녕하세요 🙂","selector":{"resourceId":"com.example:id/search"}}` | 선택한 입력란의 전체 내용 교체. 선택자 생략 시 포커스된 입력란 사용 |

`serial`은 `list_devices` 또는 `GET /api/devices`가 반환한 전역 기기 ID를 그대로 사용합니다. `rawSerial`은 실제 ADB 시리얼, `agentId`는 연결된 PC입니다. 다른 PC에 같은 에뮬레이터 시리얼이 있어도 전역 ID는 다릅니다.

조작 도구와 화면 캡처는 이 MCP 프로세스에서 점유한 기기에만 동작합니다. 에이전트가 점유 토큰을 직접 전달할 필요는 없습니다. `press_key`는 `HOME`, `BACK`, `ENTER`, `APP_SWITCH`, `DEL`, `TAB`, `ESCAPE`, `POWER`, `VOLUME_UP`, `VOLUME_DOWN`, `DPAD_UP/DOWN/LEFT/RIGHT` 또는 숫자 문자열을 받습니다.

좌표는 축소된 미리보기가 아닌 **PNG 원본의 물리 픽셀** 기준입니다. `type_text` 전에 입력란을 탭하세요. 텍스트는 추가가 아닌 전체 교체이며, 접근성으로 편집할 수 없는 입력란은 오류를 반환합니다. 입력 명령 성공은 Android가 명령을 받아들였다는 뜻입니다. 화면 전환이 끝났는지는 다음 캡처로 확인하세요.

점유 시간은 기본 300초, 범위는 30–3600초입니다. MCP는 자동 갱신하지 않으며, 긴 작업은 `renew_device`를 호출해야 합니다. 정상 종료나 stdin 종료 시 반납을 시도합니다. 강제 종료나 네트워크 장애로 반납하지 못해도 TTL이 끝나면 점유가 해제됩니다.

## 터미널에서 REST 호출

다음 예제는 `curl`과 `jq`가 필요합니다. 발급한 개인 토큰과 점유 토큰은 서로 다릅니다. 개인 토큰은 `Authorization`, 기기를 점유한 뒤 받은 토큰은 `X-Lease-Token` 헤더에 넣습니다. Bearer 인증에는 브라우저 쿠키와 CSRF 토큰이 필요하지 않습니다.

```sh
export MARIONETTE_URL=http://127.0.0.1:8080
# 토큰을 명령 기록에 남기지 않고 입력
read -r MARIONETTE_TOKEN
export MARIONETTE_TOKEN

curl --fail-with-body -sS "$MARIONETTE_URL/api/devices" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" | jq

SERIAL=office-mac.ZW11bGF0b3ItNTU4MA  # 실제 목록의 serial로 변경
LEASE_TOKEN=$(curl --fail-with-body -sS -X POST \
  "$MARIONETTE_URL/api/devices/$SERIAL/lease" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"ttlSeconds":300}' | jq -er .token)

curl --fail-with-body -sS -X POST "$MARIONETTE_URL/api/devices/$SERIAL/start" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" -H "X-Lease-Token: $LEASE_TOKEN"

curl --fail-with-body -sS -X POST "$MARIONETTE_URL/api/devices/$SERIAL/actions" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" -H "X-Lease-Token: $LEASE_TOKEN" \
  -H 'Content-Type: application/json' -d '{"action":"key","keyCode":3}'

curl --fail-with-body -sS "$MARIONETTE_URL/api/devices/$SERIAL/screenshot" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" -H "X-Lease-Token: $LEASE_TOKEN" \
  -o screenshot.png

curl --fail-with-body -sS -X DELETE "$MARIONETTE_URL/api/devices/$SERIAL/lease" \
  -H "Authorization: Bearer $MARIONETTE_TOKEN" -H "X-Lease-Token: $LEASE_TOKEN"
unset LEASE_TOKEN MARIONETTE_TOKEN
```

요청 경로의 시리얼에 특수 문자가 있으면 URL 인코딩해야 합니다. Go `client` 패키지는 경로 인코딩과 헤더를 처리합니다.

| API | 설명 |
| --- | --- |
| `GET /api/devices` | 일반 사용자는 본인에게 배정된 기기만, 관리자는 전체 기기 |
| `GET /api/devices/{serial}/control` | WebSocket 입력. 최초 `{type:"auth",token}`으로 점유 확인 후 `{id,action,...}` 입력과 ACK 전달 |
| `POST /api/devices/{serial}/lease` | `{ttlSeconds}`로 점유, `{id,token,owner,expiresAt}` 반환 |
| `PUT /api/devices/{serial}/lease` | 같은 본문과 점유 헤더로 갱신 |
| `DELETE /api/devices/{serial}/lease` | `X-Lease-Token`으로 반납. `{"wipe":true}` 또는 `?wipe=1`을 보내면 정리를 시작하고 202를 반환합니다. |
| `POST /api/devices/{serial}/start` | 드라이버 시작 |
| `POST /api/device/occupy` | `{platform,tags,model,deviceId,ttlSeconds}`로 조건 배정·드라이버 시작, `{assigned,device,lease}` 반환 |
| `POST /api/devices/{serial}/actions` | `info`, `tap`, `swipe`, `key`, `text`, `keyframe`, `uiTree`, `findElement`, `click` |
| `GET /api/devices/{serial}/screenshot` | 점유 확인 후 PNG 캡처, `image/png`, `Cache-Control: no-store` |

`actions`의 필드와 응답 형식은 [PROTOCOL.md](../PROTOCOL.md)를 참고하세요. HTTP 200이어도 기기 드라이버가 거절하면 `{"ok":false,"error":"..."}`를 반환하므로 `ok`도 확인해야 합니다. Go 클라이언트와 MCP는 이를 오류로 처리합니다. HTTP 오류는 `{"error":"..."}`이며 인증 실패는 401, 점유 권한 실패는 403, 점유 충돌은 409입니다.

Go에서는 `marionette/client`의 `New`, `Devices`, `Acquire`, `Occupy`, `Renew`, `Release`, `ReleaseWith`, `Start`, `Action`, `Screenshot`, `FindElement`, `Click`, `SetText`, `UITree`, `Apps`, `LaunchApp`을 사용할 수 있습니다. 모든 호출은 `context.Context`를 받습니다. `ReleaseWith(ctx, serial, token, true)`는 반납 시 정리를 요청합니다. 전용 CLI, 대시보드에서 ADB로 점유 전달과 Kafka 작업은 [자동화 사용법](AUTOMATION.md)에 정리했습니다.

## 관리자 연결 관리

대시보드에서 **정리 실패** 기기를 열어 **다시 정리** 또는 **정리 없이 복귀**를 선택할 수 있습니다. 관리자 세션과 `X-CSRF-Token`을 사용하는 API는 각각 `POST /api/admin/devices/{serial}/cleanup`과 `POST /api/admin/devices/{serial}/restore`입니다.

관리자는 기기 모니터링에서 기기를 열거나 **에이전트 상세 → 제어 열기**에서 제어 방식을 선택할 수 있습니다.

- **일반 점유**는 일반 사용자와 같은 방식으로 빈 기기를 점유합니다.
- **점검 모드**는 확인 후 기존 사용자의 화면과 조작 연결을 종료하고, 점검을 시작한 관리자에게만 제어 권한을 줍니다. 기존 점유 토큰은 즉시 만료됩니다. 점검을 종료하거나 시간이 만료되면 다시 배정할 수 있습니다.
- **임시 제어**는 빈 기기에서 바로 시작합니다. 최대 2분 뒤 자동 반납되며 시간이 갱신되지 않습니다.

관리 API는 관리자 브라우저 세션과 `X-CSRF-Token`을 사용합니다. `POST /api/admin/devices/{serial}/control-mode`에 `{mode,leaseId}`를 보냅니다. `mode`는 `maintenance` 또는 `temporary`입니다. `leaseId`는 `GET /api/devices`에서 확인한 공개 점유 ID이며, 빈 기기에는 빈 문자열을 보냅니다. 확인한 점유와 현재 점유가 다르면 409를 반환합니다. 새 점유 응답은 기존 필드에 `mode`를 포함합니다. 점검은 `PUT /api/devices/{serial}/lease`로 갱신할 수 있으며 두 모드 모두 같은 경로에 `DELETE`를 보내 종료합니다. 두 모드의 제어 권한은 관리자 세션에만 부여됩니다.

**연결 끊기**는 현재 점유를 해제합니다. 공개 점유 ID를 `{leaseId}`로 보내 `DELETE /api/admin/devices/{serial}/lease`를 호출합니다. 기존 관리 클라이언트는 같은 경로에 `POST`로 `{leaseId,ttlSeconds}`를 보내 일반 점유를 가져올 수 있습니다.

## 토큰 저장과 배포

중계 백엔드는 개인 API 토큰의 해시와 소유자 정보를 기본 `.marionette/tokens.json`에 원자적으로 저장합니다. 새 디렉터리는 0700, 파일은 0600 권한으로 생성합니다. 서버 재시작 후에도 같은 토큰을 사용할 수 있습니다. `--token-file /persistent/path/tokens.json`으로 위치를 바꿀 수 있습니다. 여러 백엔드 프로세스가 같은 JSON 파일을 동시에 수정하는 구성은 지원하지 않습니다. PC 에이전트 등록 토큰은 별도이며 개인 API 토큰으로 에이전트를 등록할 수 없습니다.

토큰 관리 API는 브라우저 로그인과 CSRF 검증을 사용합니다.

- `GET /api/tokens`: 현재 사용자의 토큰 메타데이터 목록
- `POST /api/tokens`: `{name,expiresInDays}` → `{id,name,createdAt,expiresAt,token}`; 원문은 이 응답에만 포함
- `DELETE /api/tokens/{id}`: 현재 사용자의 토큰 폐기

설정 창은 `config.js`의 `MARIONETTE_API_URL` 또는 `marionette-api` meta에 지정한 중계 백엔드를 사용합니다. MCP 실행 파일은 웹 정적 파일과 별도로 배포할 수 있습니다.
