# Marionette

Android 기기의 화면을 브라우저로 확인하고 원격으로 조작하는 디바이스 팜입니다. OIDC 로그인, 관리자 모니터링, 독점 점유, 조건별 기기 배정, REST·CLI·Go SDK·MCP, 원격 ADB와 선택적인 Kafka 작업 큐를 제공합니다. [토스의 Nebula 소개](https://toss.tech/article/device-farm-nebula)에서 구조를 참고했으며, 해당 서비스의 소스나 내부 프로토콜을 사용하지 않습니다.

**프론트엔드 / 중계 백엔드 / PC 에이전트**를 각각 독립된 프로그램으로 실행합니다. 여러 컴퓨터에 USB로 연결된 Android 기기를 하나의 콘솔에서 관리합니다. Android는 자체 Java 드라이버를 사용하며 scrcpy·Appium에 의존하지 않습니다.

## 구성

| 구성 요소 | 실행 파일 / 소스 | 설치 위치와 역할 |
| --- | --- | --- |
| 프론트엔드 | `marionette-web`, `web/` | 웹 서버·정적 호스팅. 로그인 UI, 기기 목록, WebCodecs 영상과 조작 화면 |
| 중계 백엔드 | `marionette-backend` | 중앙 서버. OIDC, 세션·개인 API 토큰, 에이전트 인증·등록, 기기 목록 통합, REST·영상 중계 |
| PC 에이전트 | `marionette-agent` + `marionette-device.jar` | 휴대폰이 연결된 컴퓨터. ADB 검색, 점유·관리자 권한 적용, Android 드라이버·영상·입력 관리 |
| CLI / MCP | `marionette`, `marionette-mcp` | 사용자 PC. 조건 배정, 네이티브 UI 자동화, ADB 연결 또는 에이전트 도구 제공 |
| 작업 러너 | `marionette-runner` | Kafka 작업 소비. 여러 프로세스가 같은 그룹에서 기기를 배정받아 단계를 실행 |

```text
브라우저 ── 정적 파일 ── 프론트엔드 :3000
   │
   └── HTTP / WebSocket ── 중계 백엔드 :8080 ←── REST / MCP 클라이언트
                                  ▲
                           에이전트가 여는 WSS 터널
                             ┌────┴────┐
                        PC 에이전트 A  PC 에이전트 B
                             │             │
                            ADB           ADB
                             │             │
                        Android 기기   Android 기기
```

프론트엔드와 백엔드는 ADB나 Android SDK를 사용하지 않습니다. 에이전트는 수신 포트를 열지 않고 백엔드에 연결하므로 PC가 NAT 뒤에 있어도 동작합니다. 터널 한 개에 HTTP·영상 스트림을 다중화하며, 에이전트 연결이 끊기면 해당 PC의 점유와 기기 런타임을 종료하고 재접속합니다.

## 실행

개발 빌드에는 Go 1.26 이상, Android 드라이버 도구 설치에는 Python 3.12 이상이 필요합니다. Node.js 20 이상은 테스트에 사용하며 웹 콘솔에는 별도의 패키지 설치나 번들 빌드가 없습니다. 배포 패키지를 설치하는 PC에는 ADB만 필요합니다.

먼저 OIDC 제공자에 **confidential web client**를 등록합니다. Authorization Code와 PKCE S256을 지원해야 하며, 로컬 개발 콜백 주소는 `http://127.0.0.1:8080/auth/callback`로 정확히 등록하세요. 제공자의 계정·그룹 정책에서 이 클라이언트를 사용할 사용자를 제한할 수 있습니다.

```sh
cp .env.example .env
# .env의 OIDC, 관리자 목록, 프론트엔드 주소, 에이전트 등록 정보 수정
# PC마다 별도 토큰을 생성하고 백엔드와 해당 PC에 같은 값을 설정
openssl rand -hex 32
```

| 환경 변수 | 값 |
| --- | --- |
| `MARIONETTE_OIDC_ISSUER` | 제공자의 discovery issuer URL |
| `MARIONETTE_OIDC_CLIENT_ID` | 등록한 web client ID |
| `MARIONETTE_OIDC_CLIENT_SECRET` | 해당 클라이언트의 서버용 secret |
| `MARIONETTE_OIDC_REDIRECT_URL` | 등록한 콜백 URL, 경로는 `/auth/callback` |
| `MARIONETTE_ADMIN_EMAILS` | 쉼표로 구분한 관리자 이메일. 제공자가 `email_verified: true`로 확인한 주소만 적용 |
| `MARIONETTE_ADMIN_SUBJECTS` | 쉼표로 구분한 관리자 OIDC `sub` 값. 대소문자를 포함해 정확히 일치해야 함 |
| `MARIONETTE_FRONTEND_ORIGINS` | 백엔드에서 허용할 프론트엔드 오리진. 쉼표로 구분하며 첫 항목이 로그인 후 이동 주소. 로컬 설정 `http://127.0.0.1:3000` |
| `MARIONETTE_AGENT_TOKENS` | 백엔드에 등록할 `{"office-mac":"<32바이트 이상 토큰>","lab-linux":"<별도 토큰>"}` JSON 객체 |
| `MARIONETTE_API_URL` | 프론트엔드가 접속할 백엔드 오리진. 로컬 설정 `http://127.0.0.1:8080` |

관리자 목록에 없는 로그인 사용자는 멤버가 됩니다. 한 계정은 모든 에이전트를 통틀어 동시에 한 기기만 점유할 수 있습니다. 멤버는 배정된 본인 기기의 목록·화면·정보만 조회하고, 관리자는 점유하지 않고도 전체 기기를 모니터링합니다. 관리자 목록이 비어 있으면 모두 멤버입니다. 일반 OAuth2 로그인만 제공하고 OIDC ID 토큰을 발급하지 않는 제공자는 지원하지 않습니다.

```sh
# 프로젝트의 .tools/ 아래에 JDK 17, Android SDK 34, D8, ADB 설치
make tools

# Android에서 실행할 DEX JAR 빌드
make device

# .env는 셸 문법입니다. 공백이 있는 값은 따옴표로 감싸세요.
# 실행 스크립트가 자동으로 읽지 않으므로 환경 변수로 내보냅니다.
set -a
. ./.env
set +a

# 터미널 1: 중계 백엔드 (ADB와 웹 파일 불필요)
make run-backend
```

```sh
# 터미널 2: 프론트엔드
export MARIONETTE_API_URL=http://127.0.0.1:8080
make run-web
```

```sh
# 터미널 3: 휴대폰이 연결된 PC
# 토큰은 백엔드의 MARIONETTE_AGENT_TOKENS에 등록한 것과 일치해야 함
export MARIONETTE_BACKEND_URL=http://127.0.0.1:8080
export MARIONETTE_AGENT_ID=office-mac
read -r MARIONETTE_AGENT_TOKEN
export MARIONETTE_AGENT_TOKEN
make run-agent
```

브라우저에서 **<http://127.0.0.1:3000>**을 엽니다. H.264 WebCodecs를 지원하는 최신 Chrome을 권장합니다. Android의 개발자 옵션에서 USB 디버깅을 켜고, USB 케이블로 연결한 뒤 기기에 표시되는 컴퓨터 인증을 허용하세요. 다른 컴퓨터에는 `make agent-package`로 만든 패키지를 설치합니다. [PC 에이전트 설치 안내](docs/AGENT_INSTALL.md)에 환경 설정과 실행 방법을 정리했습니다.

**SSO로 로그인**을 누르면 제공자 로그인 화면으로 이동합니다. OIDC 설정이 없으면 로그인 안내만 표시하고 기기 API 접근은 차단합니다. 설정 네 항목 중 일부만 입력하면 서버 시작 시 오류를 반환합니다.

관리자 화면은 어두운 보드 위에 기기 화면을 여러 열로 배치합니다. 검색, 상태 필터, 화면 크기 조절, 일시정지를 지원하며, 화면 밖의 영상과 숨겨진 탭의 디코더·연결은 중단합니다. 기기 목록은 실제 ADB 검색 결과를 사용합니다.

멤버는 **기기 배정받기**를 눌러 사용 가능한 한 기기에 연결합니다. 관리자는 기기를 열고 **점유하고 조작**을 누릅니다. 화면 클릭과 드래그는 전용 WebSocket으로 down/move/up 이벤트를 즉시 전달하며, hover도 HTTP 요청마다 기다리지 않고 최신 위치를 보냅니다. 앱·프로세스 조회와 입력 경로를 분리했습니다. 사용을 마치면 **기기 반납**을 누릅니다. 새로고침이나 상세 창 닫기는 점유를 반납하지 않습니다. 로그인한 계정의 서버 점유를 다시 확인해 같은 세션으로 복구하며, 새로고침 전에 열어 둔 조작 화면도 다시 연결합니다. 점유는 기본 5분이며 로그인한 대시보드가 열려 있는 동안 상세 창을 닫아도 갱신합니다. 페이지를 떠난 뒤 갱신하지 않으면 TTL 만료 시 해제되며, 명시적 반납·로그아웃·관리자 해제도 점유를 종료합니다. 점유 토큰은 로그인 사용자의 OIDC subject에도 묶이므로 다른 계정이 사용할 수 없습니다.

반납할 때 **그대로 반납** 또는 **정리하고 반납**을 선택할 수 있습니다. 정리는 설치한 앱과 내부 저장소(`/sdcard`)의 사용자 파일을 지웁니다. 공장 초기화는 하지 않으므로 기기 계정 로그인 같은 시스템 설정은 남을 수 있습니다.

| 용도 | 최대 긴 변 | 목표 프레임 상한 | 목표 비트레이트 |
| --- | ---: | ---: | ---: |
| 관리자 미리보기 | 480px | 8fps | 600kbps |
| 점유한 조작 화면 | 1280px | 30fps | 4Mbps |

기기당 캡처 프로세스와 인코더를 공유하며, 조작을 시작하면 높은 품질로 전환하고 반납 후 미리보기 품질로 돌아옵니다. 조작 중인 기기를 다른 관리자가 보면 같은 높은 품질의 스트림을 공유합니다. 품질 전환은 실제 MediaCodec을 재구성하며, 프레임 수치와 비트레이트는 보장 성능이 아닌 설정 목표입니다. 브라우저는 동시에 최대 36개 미리보기를 재생하며 스크롤에 따라 연결을 바꿉니다.

백엔드 포트를 바꾸면 콜백 등록과 `MARIONETTE_OIDC_REDIRECT_URL`, 프론트엔드의 API 주소, 에이전트의 백엔드 주소도 함께 변경하고 `bash scripts/run.sh backend --listen 127.0.0.1:8081`로 실행하세요. 프론트엔드 포트는 `bash scripts/run.sh web --listen 127.0.0.1:3001`로 바꾸고 백엔드의 허용 오리진도 수정합니다. `127.0.0.1`과 `localhost`는 서로 다른 오리진이므로 로컬에서는 둘 중 하나로 통일하세요.

기존 도구를 사용하려면 `JAVA_HOME`, `ANDROID_SDK_ROOT`를 설정하고 `make device`를 실행하세요. SDK에는 `platforms;android-34`와 `build-tools;34.0.0`이 있어야 합니다. 에이전트 실행 시에는 `adb`가 PATH에 있어야 합니다. `scripts/run.sh`는 로컬 `.tools/`의 ADB도 찾아 사용하며, `--adb /absolute/path/to/adb`로 직접 지정할 수도 있습니다.

## API와 MCP로 조작

Claude 같은 MCP 클라이언트는 `<백엔드 주소>/mcp`를 원격 MCP 서버로 추가하면 됩니다. 토큰 없이 OAuth 2.1(PKCE, 동적 클라이언트 등록)로 로그인·허용하고, 웹 콘솔 **API / MCP**의 **연결된 앱**에서 폐기합니다. CLI·REST·로컬 stdio MCP는 같은 창에서 개인 토큰을 발급해 사용합니다.

```sh
make mcp  # build/marionette-mcp 생성
```

MCP는 `기기 배정 → 화면 캡처·탭·스와이프·키·텍스트 → 반납` 흐름을 제공합니다. 기기 목록에는 본인에게 배정된 기기만 표시되므로, 목록이 비어 있으면 `acquire_device`에서 시리얼을 생략해 배정받습니다. 브라우저와 같은 계정당 한 기기 점유 정책을 사용합니다. 점유 토큰은 MCP 프로세스에서 관리하고, 종료 시 반납을 시도합니다. 점유는 기본 5분이며 MCP에서는 명시적으로 갱신해야 합니다.

같은 개인 토큰으로 `curl`이나 공개 Go `client` 패키지에서도 REST API를 호출할 수 있습니다. 브라우저 세션과 CSRF 토큰 없이 `Authorization: Bearer ...` 헤더로 인증합니다. 실행 설정, 전체 도구 목록, CLI 예제는 [API / MCP 사용법](docs/API_MCP.md)에 정리했습니다.

```sh
make cli
build/marionette occupy --platform android --tags lab
build/marionette click --id com.example.app:id/search
build/marionette type --id com.example.app:id/search --value '안녕하세요 🙂'
build/marionette adb  # 배정된 기기를 로컬 ADB / Android Studio에 연결
```

기기 ID를 미리 알지 못해도 태그·모델로 배정받을 수 있습니다. Android의 ID 탐색·UI XML은 실행 중인 드라이버에서 처리합니다. [자동화·원격 ADB·Kafka 안내](docs/AUTOMATION.md)에 CLI 설정, 브라우저에서 점유 넘기기, 작업 제출과 여러 러너 실행 방법을 정리했습니다.

## 로그인과 배포

[배포 환경 변수](docs/CONFIG.md)에서 공개 주소, 관리자, 상태 경로와 실행 파일별 설정을 확인할 수 있습니다.

백엔드와 웹 콘솔을 단일 HTTPS 주소로 운영하는 방법은 [Docker Compose 배포 안내](docs/DEPLOY.md)를 참고하세요.

Ubuntu PC는 관리자 대시보드의 **에이전트 운영 → 에이전트 연결 → Ubuntu (권장)**에서 복사한 명령 한 줄로 설치하고 연결할 수 있습니다. 자동 시작 설정은 [PC 에이전트 설치 안내](docs/AGENT_INSTALL.md)와 [운영 서비스 설정](deploy/README.md)을 참고하세요. macOS는 사용자 로그인 후 LaunchAgent로 실행하고, Ubuntu는 systemd 사용자 서비스와 linger로 부팅 시 실행합니다. 설정 생성과 실제 서비스 등록·재부팅 확인은 별도 단계입니다.

로그인은 Authorization Code + PKCE S256을 사용하고 state, nonce, ID 토큰의 서명·issuer·audience를 검증합니다. 브라우저에는 HttpOnly 세션 쿠키를 발급하며, 변경 요청에는 별도의 CSRF 토큰을 요구합니다. HTTPS 콜백을 설정하면 Secure 쿠키를 사용합니다. 로그아웃과 세션 만료는 기존 영상 연결도 종료합니다. 현재 로그아웃은 Marionette 세션을 종료하며, 회사 SSO 제공자의 세션까지 로그아웃시키지는 않습니다.

세션은 메모리에 저장하며 기본 유효기간은 8시간입니다. 서버 재시작 시 다시 로그인해야 합니다. 여러 서버 인스턴스가 세션을 공유하는 구성은 아직 포함하지 않습니다.

개인 API 토큰은 기본 30일, 최대 90일 동안 유효하고 웹 콘솔에서 폐기할 수 있습니다. 관리자가 발급해도 API 권한은 일반 사용자와 같습니다. 토큰 원문은 발급할 때 한 번만 표시하고 SHA-256 해시와 소유자 정보만 `.marionette/tokens.json`에 저장합니다. `--token-file`로 저장 경로를 바꿀 수 있으며, 컨테이너에서는 이 경로를 영속 볼륨에 둡니다. 브라우저 로그아웃은 개인 토큰을 폐기하지 않습니다.

백엔드와 프론트엔드는 기본적으로 loopback에 바인딩합니다. 외부 접속은 HTTPS reverse proxy를 앞에 두고, 백엔드 공개 주소의 `/auth/callback`을 등록하세요. proxy는 HTTP Host와 WebSocket Upgrade를 보존해야 하며 `/internal/agents/{id}/connect`도 백엔드로 전달합니다. 허용되는 공개 Host는 `MARIONETTE_PUBLIC_URL`에서 정하며, 이 값이 비어 있으면 콜백 주소를 사용합니다. `X-Forwarded-Host`는 사용하지 않습니다. 서로 다른 사이트에 배포할 때 백엔드는 HTTPS여야 하며 브라우저의 교차 사이트 쿠키 정책이 적용됩니다. 가능하면 `console.example.com` / `api.example.com`처럼 같은 사이트를 사용하세요.

프론트엔드는 `make frontend`로 만든 서버와 `web/`를 함께 배포하거나, `web/`만 정적 호스팅합니다. 서버 사용 시 `MARIONETTE_API_URL`을 런타임에 읽고, 정적 호스팅 시 `web/config.js`에 백엔드 주소를 지정합니다. 정적 호스팅에는 `/admin`, `/devices`, `/login`의 `index.html` 별칭을 설정하세요.

서버 Docker 이미지는 Ubuntu 설치용 Linux amd64·arm64 패키지와 체크섬을 `/agent-packages`에 포함합니다. 백엔드를 직접 실행하는 환경에서는 `make agent-package-linux`로 만든 파일을 `--agent-packages` 경로(환경 변수 `MARIONETTE_AGENT_PACKAGES`, 기본 `build`)에 배치합니다. 설치 스크립트는 기본 `scripts/setup-ubuntu-agent.sh`를 사용하며 `--install-script` 또는 `MARIONETTE_INSTALL_SCRIPT`로 지정할 수 있습니다. HTTPS 프록시에서 `/install/ubuntu.sh`와 `/downloads/*`를 백엔드에 전달하면 설치 PC가 로그인 없이 파일을 받을 수 있습니다.

백엔드는 `make backend`로 만든 실행 파일과 인증 설정 외에 영속 데이터도 보존해야 합니다. 개인 토큰은 `--token-file`(기본 `.marionette/tokens.json`), 대시보드에서 등록한 PC 정보·토큰 해시는 `--agent-file`(기본 `.marionette/agents.json`)에 저장합니다. 관리자 화면의 회원 목록용 기록(subject·이메일·이름·로그인/접속 시각만, 토큰·비밀 없음)은 `--user-file`(기본 `.marionette/users.json`)에 저장합니다. Kafka 작업 큐를 사용하면 `--job-file`(기본 `.marionette/jobs.json`)과 그 파일의 부모 디렉터리 아래 `artifacts/`도 함께 보존합니다. 기본 경로를 쓰는 컨테이너에서는 `.marionette/` 전체를 영속 볼륨에 둡니다. 이 JSON 저장소들은 한 백엔드 프로세스만 쓰도록 운영하며, 여러 백엔드가 같은 파일을 공유하는 구성은 지원하지 않습니다. 에이전트에는 OIDC client secret을 배포하지 않습니다.

## 기기 런타임

```text
PC 에이전트 (중계 백엔드에서 인증한 사용자 식별 정보를 수신)
  ├─ ADB: 기기 검색, JAR 전송, app_process 실행, 포트 포워딩
  ├─ 사용자별 점유와 관리자 미리보기 관리
  └─ 캡처 품질 전환, 영상 수신 및 백엔드를 통한 브라우저 전달
          │ ADB forward → Android abstract local socket
Android Java 드라이버 (shell 권한)
  ├─ 디스플레이 캡처 → Surface → MediaCodec H.264
  ├─ 입력 이벤트 주입: 탭, 스와이프, 키
  └─ UiAutomation: 접근성을 통한 텍스트 설정
```

영상과 제어 규격은 [PROTOCOL.md](PROTOCOL.md), Android 구현과 호환성 주의점은 [device/README.md](device/README.md)에 정리합니다. 프레임을 JPEG로 반복 캡처하는 방식 대신 MediaCodec의 H.264 출력을 사용합니다. 브라우저는 WebCodecs로 디코딩하고 Canvas에 표시합니다.

## 현재 범위와 제약

- Android 11–14(API 30–34)를 구현 목표로 삼습니다. 제조사·OS별 동작 검증이 완료된 지원 목록을 의미하지 않습니다.
- 화면 캡처와 입력 주입에 Android의 비공개 시스템 API를 사용합니다. 일반 APK 설치와 공개 SDK 권한만으로 실행하는 앱이 아닙니다. ADB shell의 `app_process`에서 구동하며, API 변경에 맞춰 호환성 코드를 유지해야 합니다.
- 한글·이모지는 포커스된 입력란에 접근성 `ACTION_SET_TEXT`로 설정합니다. 기존 내용을 교체하며, 접근성으로 편집할 수 없는 커스텀 입력란에는 실패를 반환합니다. 전용 IME와 동일한 지원 범위를 제공하지 않습니다.
- 보호된 화면(`FLAG_SECURE`), 잠금 화면, 제조사의 입력 차단 정책을 우회하지 않습니다. 오디오와 다중 터치는 포함하지 않습니다. 파일 전송·앱 설치는 점유한 Android의 ADB 중계로 사용합니다.
- 잠금이나 통신 중단으로 ADB 목록에서 빠진 기기도 현재 연결된 에이전트의 목록에 오프라인으로 남깁니다. macOS와 Linux에서는 USB에 Android 기기가 잡히면 ADB 연결 전에도 `디버깅 대기`로 표시합니다. USB 감지는 잠금 여부나 디버깅 허용 여부를 뜻하지 않으며, 화면 연결과 조작은 ADB가 `device` 상태일 때 시작합니다.
- 드라이버 단독 실행 기본값은 긴 변 1280, 목표 30fps, 4Mbps입니다. 관리자 그리드는 위의 미리보기 프로필을 사용합니다. 실제 성능은 기기 인코더, USB, 호스트와 브라우저에 따라 달라집니다. Nebula의 성능 수치를 재현했다고 주장하지 않습니다.
- 마우스 hover는 Android의 실제 마우스 이벤트로 전달합니다. 앱 자체가 hover를 지원해야 해당 앱의 설명이 나타납니다. 드래그는 전용 WebSocket의 down/move/up 이벤트로 전달하고, 연결이 끊기면 눌린 입력을 취소하며 이전 입력을 자동 재전송하지 않습니다.
- 여러 PC의 기기를 통합하며 같은 ADB 시리얼도 에이전트별로 구분합니다. API의 `serial`은 기기 목록에서 받은 전역 ID를 사용합니다. 원래 ADB 시리얼은 `rawSerial`, 연결된 PC는 `agentId`입니다.
- 관리자 대시보드에서 에이전트를 등록하고 PC별 실물·가상·혼합 운영, 관리용 에뮬레이터 수와 컨트롤러 상시 대기를 설정합니다. 기존 환경 변수 등록도 지원합니다. Kafka 큐와 여러 러너를 지원하지만 백엔드의 토큰·PC 등록·작업 저장소는 단일 백엔드 프로세스가 쓰는 JSON입니다. 여러 백엔드가 공유하는 분산 DB와 장애 인계는 포함하지 않습니다. PC용 배포 패키지는 macOS와 Linux를 지원합니다.
- Android Studio의 IDE 디버깅 UI는 이번 검증 범위에 포함하지 않습니다. 실제 ADB 중계의 셸·파일 전송·캡처와 반납 후 연결 종료는 확인했습니다.

## 개발 및 검증

```sh
make test    # Go race detector 포함 테스트 + 브라우저 프로토콜/디코더 테스트
make check   # Go vet, JS/Python 구문 확인
make backend agent frontend  # 각 구성 요소의 독립 실행 파일
make agent-package           # PC 설치용 실행 파일 + Android JAR + 설정 예제
make agent-package-linux     # Ubuntu용 amd64·arm64 패키지 + systemd 서비스
make mcp     # build/marionette-mcp 생성
make cli runner  # 사용자 CLI와 Kafka 작업 러너
```

2026-11-16에 Android 14/API 34 공식 arm64 에뮬레이터에서 실제 H.264 디코딩, 탭·스와이프·키 입력, 한글·일본어·이모지 입력, 화면 회전과 재연결을 확인했습니다. 이후 실제 RS256 ID 토큰과 PKCE를 쓰는 로컬 테스트 제공자로 로그인하고, Go 호스트와 브라우저를 거쳐 미리보기 270×480 → 조작 720×1280 → 홈 → 반납 후 270×480 → 로그아웃을 확인했습니다.

27개 영상 타일의 배치는 녹화된 실제 AVC 데이터를 재생하는 테스트 기기로 검증했습니다. 이는 27대 실기기 부하 검증이 아닙니다. 1440px 화면의 9열 그리드와 390px 모바일의 2열 그리드에 가로 넘침이 없었고, 일시정지 후 테스트 영상 연결이 0개가 됐습니다. 일반 사용자 계정의 관리자 미리보기 접근은 403으로 차단됐습니다. 사용자의 실제 OIDC 제공자·자격 증명 연결은 수행하지 않았습니다.

상세 기록: [로그인·대시보드 검증](docs/AUTH_DASHBOARD_VALIDATION.md), [기존 드라이버 검증](device/EMULATOR_VALIDATION.md), [네이티브 품질 전환 검증](device/PREVIEW_VALIDATION.md), [실기기 자동화와 ADB 확인 범위](docs/AUTOMATION.md#확인한-범위).

위 기록은 기존 결합형 호스트의 검증 이력입니다. 분리 구조는 두 에이전트의 동일 시리얼 구분, HTTP·WebSocket 중계, 2.1MB 영상 패킷 전달, 신원 헤더 위조 차단, 점유 소유자·관리자 권한, 인증 세션 종료 시 영상 종료를 통합 테스트로 확인합니다.

2026-11-17에 빌드된 프론트엔드·백엔드와 압축 패키지에서 설치한 두 에이전트를 저장소 밖에서 각각 별도 프로세스로 실행했습니다. API 주소 주입, 라우트 분리, OIDC 설정 전 CORS, 중복 시리얼 통합, JAR 탐지, 점유, 에이전트 종료·재접속과 이전 점유 토큰 무효화를 확인했습니다. 이 실행 검증의 ADB 검색은 격리된 테스트 데이터를 사용했으며 실제 휴대폰을 조작하지 않았습니다.

실기기에서는 화면 표시 → 탭/스와이프 → 홈/뒤로 → 한글 입력 → 화면 회전 → 반납 → 재연결 → USB 분리 순서로 확인해야 합니다. 빌드와 프로토콜 테스트만으로 캡처·입력의 기기 호환성을 보장할 수 없습니다.

## 다음 확장

Android 버전·제조사별 검증 목록, 전용 IME와 기기 복구 정책을 확장할 수 있습니다. 백엔드 고가용성이 필요하면 JSON 저장소를 트랜잭션과 fencing을 지원하는 공유 DB로 교체해야 합니다.
