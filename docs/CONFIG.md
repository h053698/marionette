# 배포 환경 변수

설정은 실행 파일이 시작할 때 읽습니다. 우선순위는 **명령줄 플래그 → 환경 변수 → 기본값**입니다. 환경 변수를 내보내거나 서비스 관리자에서 주입하세요. 실행 파일은 `.env`를 직접 읽지 않습니다. 예제 파일의 비밀값은 교체하고 접근 권한을 제한하세요.

소스 설치의 `scripts/start-service.sh`는 백엔드 토큰·사용자·에이전트·작업 파일과 웹 파일 경로를 플래그로 전달합니다. 해당 경로를 환경 변수만으로 지정할 때는 서비스 관리자가 실행 파일을 직접 호출하도록 구성하세요. `scripts/start-agent.sh`는 패키지나 빌드 디렉터리의 JAR를 플래그로 전달하므로 별도 JAR는 실행 파일에 `MARIONETTE_DEVICE_JAR`로 제공하거나 스크립트에 `--device-jar`로 전달하세요.

## 백엔드 `marionette-backend`

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_LISTEN` | `127.0.0.1:8080` | `0.0.0.0:8080` | 선택 | 시작 시 HTTP 바인딩 |
| `MARIONETTE_PUBLIC_URL` | OIDC 콜백의 오리진 | `https://api.example.com` | OIDC 사용 시 콜백과 둘 중 하나 필수 | 시작 시 공개 API 및 MCP OAuth 주소 |
| `MARIONETTE_FRONTEND_ORIGINS` | 빈 목록 | `https://devices.example.com` | 웹을 별도 오리진에 호스팅할 때 필요 | 시작 시 CORS 및 로그인 복귀 주소 |
| `MARIONETTE_OIDC_ISSUER` | 없음 | `https://sso.example.com/realms/team` | SSO 사용 시 필수 | 시작 시 OIDC 검색 |
| `MARIONETTE_OIDC_CLIENT_ID` | 없음 | `marionette` | SSO 사용 시 필수 | 시작 시 OIDC 클라이언트 구성 |
| `MARIONETTE_OIDC_CLIENT_SECRET` | 없음 | 발급받은 비밀값 | SSO 사용 시 필수 | 시작 시 OIDC 클라이언트 구성 |
| `MARIONETTE_OIDC_REDIRECT_URL` | 공개 URL + `/auth/callback` | `https://api.example.com/auth/callback` | 공개 URL을 생략하고 SSO를 쓸 때 필수 | 시작 시 OIDC 클라이언트 구성 |
| `MARIONETTE_ADMIN_EMAILS` | 빈 목록 | `admin@example.com,ops@example.com` | 운영 오리진에서는 두 관리자 목록 중 하나 필수 | 시작 후 다음 SSO 로그인 |
| `MARIONETTE_ADMIN_SUBJECTS` | 빈 목록 | `oidc-user-123` | 운영 오리진에서는 두 관리자 목록 중 하나 필수 | 시작 후 다음 SSO 로그인 |
| `MARIONETTE_DATA_DIR` | `.marionette` | `/var/lib/marionette` | 선택 | 시작 시 상태 파일 기본 경로 |
| `MARIONETTE_STATE_DIR` | 없음 | `/var/lib/marionette` | 선택, 이전 백엔드 시작 스크립트 호환 | `MARIONETTE_DATA_DIR` 미설정 시 시작 경로 |
| `MARIONETTE_TOKEN_FILE` | 데이터 디렉터리의 `tokens.json` | `/var/lib/marionette/tokens.json` | 선택 | 시작 시 개인 API 토큰 해시 저장소 |
| `MARIONETTE_USER_FILE` | 데이터 디렉터리의 `users.json` | `/var/lib/marionette/users.json` | 선택 | 시작 시 사용자 레지스트리 |
| `MARIONETTE_OAUTH_FILE` | 데이터 디렉터리의 `oauth.json` | `/var/lib/marionette/oauth.json` | 선택 | 시작 시 MCP OAuth 등록·갱신 토큰 저장소 |
| `MARIONETTE_AGENT_FILE` | 데이터 디렉터리의 `agents.json` | `/var/lib/marionette/agents.json` | 선택 | 시작 시 에이전트 등록 저장소 |
| `MARIONETTE_JOB_FILE` | 데이터 디렉터리의 `jobs.json` | `/var/lib/marionette/jobs.json` | 선택 | 시작 시 Kafka 작업 저장소 |
| `MARIONETTE_SESSION_TTL` | `8h` | `4h` | 선택, `1m`–`24h` | 시작 후 새 브라우저 세션 |
| `MARIONETTE_OAUTH_ACCESS_TTL` | `1h` | `30m` | 선택, `1s`–`24h` | 시작 후 새 MCP OAuth 액세스 토큰 |
| `MARIONETTE_OAUTH_REFRESH_TTL` | `720h` | `336h` | 선택, `1s`–`8760h` | 시작 후 새로 발급하거나 교체하는 MCP OAuth 갱신 토큰 |
| `MARIONETTE_OAUTH_CODE_TTL` | `5m` | `2m` | 선택, `1s`–`1h` | 시작 후 새 MCP OAuth 인증 코드 |
| `MARIONETTE_AGENT_PACKAGES` | `build` | `/opt/marionette/downloads` | 선택 | 시작 후 에이전트 패키지 다운로드 경로 |
| `MARIONETTE_INSTALL_SCRIPT` | 패키지 디렉터리 상위의 `scripts/setup-ubuntu-agent.sh`, 이후 작업 디렉터리의 같은 경로 | `/opt/marionette/scripts/setup-ubuntu-agent.sh` | 선택 | 시작 후 `/install/ubuntu.sh` 다운로드 경로 |
| `MARIONETTE_AGENT_TOKENS` | 빈 목록 | `{"lab-pc":"32바이트-이상의-개별-토큰"}` | 선택, 웹에서 에이전트 등록 가능 | 시작 시 사전 등록 |

공개 URL은 경로·쿼리가 없는 HTTPS 오리진입니다. 루프백에서만 HTTP를 허용합니다. 루프백 이외의 공개 주소에는 OIDC 발급자·클라이언트 ID·비밀값·콜백과 관리자 목록이 필요합니다. 명시한 콜백은 경로가 `/auth/callback`이어야 하고 공개 URL과 스킴 및 호스트 이름이 같아야 합니다. 공개 URL로 콜백을 자동 구성하는 경우에도 OIDC 제공자에서 정확히 일치하는 콜백을 허용해야 합니다. 프론트엔드 오리진과 관리자 목록은 쉼표 또는 줄바꿈으로 구분합니다. 프론트엔드 목록의 첫 오리진이 로그인 후 복귀 주소입니다. 로컬 개발에서는 OIDC를 전부 생략해 로그인 안내 화면을 확인할 수 있습니다.

관리자 역할은 로그인할 때 OIDC의 검증된 이메일 또는 정확히 일치하는 `sub`로 결정합니다. 브라우저 세션에는 생성 당시 역할이 유지됩니다. 환경 변수를 변경해 백엔드를 재시작하면 메모리의 브라우저 세션이 모두 종료되고, 다음 로그인부터 새 관리자 목록이 적용됩니다. 저장된 개인 API 토큰과 MCP OAuth 토큰에는 사용자 권한만 부여됩니다. 해당 접근도 회수하려면 토큰을 별도로 폐기하세요.

역방향 프록시는 외부 HTTPS의 `Host` 헤더(필요하면 공개 포트 포함)를 백엔드에 그대로 전달해야 합니다. 같은 호스트 이름에서 콜백에 별도 포트를 쓰면 해당 포트에서도 `/auth/callback`을 전달하세요. 쿠키의 `Secure`와 공개 URL은 환경 설정에서 결정합니다. `X-Forwarded-Host`, `X-Forwarded-Proto`, `Forwarded`는 이 판단에 사용하지 않습니다. 같은 호스트에서 프록시를 쓸 때는 루프백 수신을 유지하세요. 컨테이너에서는 내부에서 `0.0.0.0:8080`으로 수신하고 외부 HTTPS는 프록시에서 종료할 수 있습니다.

인증 없이 `GET /healthz`에서 HTTP 서버가 응답 중인지 확인할 수 있습니다(`200`). `GET /readyz`는 OIDC 설정을 마치고 서비스를 제공 중이면 `200`, OIDC 미설정 또는 종료 중이면 `503`을 반환합니다. 두 응답 모두 사용자·기기 데이터를 포함하지 않습니다.

작업 스크린샷은 `jobs.json`과 같은 디렉터리의 `artifacts/`에 저장됩니다. `MARIONETTE_JOB_FILE`로 위치를 바꾸면 `artifacts/`도 새 파일의 상위 디렉터리를 기준으로 찾습니다. 마이그레이션할 때 두 경로를 함께 옮기세요.

동영상 연결과 작업 보관 한도는 아래 표를 참고하세요.

## 웹 `marionette-web`

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_LISTEN` | `127.0.0.1:3000` | `0.0.0.0:3000` | 선택 | 시작 시 HTTP 바인딩 |
| `MARIONETTE_WEB_DIR` | `web` | `/opt/marionette/web` | 선택 | 시작 시 정적 파일 디렉터리 |
| `MARIONETTE_API_URL` | 페이지와 같은 오리진 | `https://api.example.com` | API가 별도 오리진이면 필요 | 시작 후 브라우저가 `/config.js`를 읽을 때 |

정적 호스팅만 사용하는 경우에는 `web/config.js`에서 API 주소를 지정합니다.

## Kafka 백엔드 및 러너 `marionette-runner`

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_KAFKA_BROKERS` | 빈 목록 | `kafka:9092` | 러너 필수, 백엔드에서는 설정 시 작업 활성화 | 시작 시 Kafka 연결 구성 |
| `MARIONETTE_KAFKA_TOPIC` | `marionette.jobs` | `farm.jobs` | 선택 | 시작 시 작업 토픽 |
| `MARIONETTE_KAFKA_TLS` | `false` | `true` | 선택, SASL/PLAIN 사용 시 `true` 필수 | 시작 시 Kafka 전송 보안 |
| `MARIONETTE_KAFKA_USERNAME` | 없음 | `marionette` | SASL/PLAIN 사용 시 필수 | 시작 시 Kafka 인증 |
| `MARIONETTE_KAFKA_PASSWORD` | 없음 | 발급받은 비밀값 | SASL/PLAIN 사용 시 필수 | 시작 시 Kafka 인증 |
| `MARIONETTE_RUNNER_TOKEN` | 없음 | 임의의 32바이트 이상 비밀값 | 러너 및 작업을 활성화한 백엔드 필수 | 시작 시 러너 인증 |
| `MARIONETTE_BACKEND_URL` | 없음 | `https://api.example.com` | 러너 필수 | 시작 시 백엔드 접속 주소 |
| `MARIONETTE_RUNNER_GROUP` | `marionette-runners` | `farm-runners` | 선택 | 시작 시 Kafka 소비자 그룹 |

백엔드와 러너의 Kafka 접속 설정과 러너 토큰을 맞춥니다. 같은 토픽을 읽는 러너는 동일 소비자 그룹을 사용합니다.

## 에이전트 `marionette-agent`

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_BACKEND_URL` | 없음 | `https://api.example.com` | 필수 | 시작 시 백엔드 접속 주소 |
| `MARIONETTE_AGENT_ID` | 없음 | `lab-pc` | 필수 | 시작 시 에이전트 인증 |
| `MARIONETTE_AGENT_TOKEN` | 없음 | PC 등록 시 발급받은 비밀값 | 필수 | 시작 시 에이전트 인증 |
| `MARIONETTE_ADB` | `adb` (PATH에서 검색) | `/opt/android/platform-tools/adb` | 선택 | 시작 시 ADB 실행 파일 |
| `MARIONETTE_DEVICE_JAR` | 실행 파일 옆 `marionette-device.jar`, 없으면 `build/marionette-device.jar` | `/opt/marionette/marionette-device.jar` | 선택 | 시작 시 Android 드라이버 경로 |
| `MARIONETTE_DEVICE_TAGS` | 빈 목록 | `lab,android` | 선택 | 시작 후 기기 검색 및 배정 태그 |
| `MARIONETTE_PLATFORM` | `android` | `android` | 선택 | 시작 시 대상 플랫폼 확인 |
| `MARIONETTE_ANDROID_SDK` | `ANDROID_SDK_ROOT` → `ANDROID_HOME` → `platform-tools` 아래 ADB 경로에서 추론 | `/opt/android` | 선택 | 시작 시 SDK 탐색 |
| `ANDROID_SDK_ROOT` | 없음 | `/opt/android` | 선택 | 시작 시 `MARIONETTE_ANDROID_SDK` 미설정 시 SDK 탐색 |
| `ANDROID_HOME` | 없음 | `/opt/android` | 선택 | 시작 시 앞선 설정이 없을 때 SDK 탐색 |
| `MARIONETTE_STATE_DIR` | `.marionette/agent-<ID>` | `/var/lib/marionette/lab-pc` | 선택 | 시작 시 에이전트 정책 및 관리 AVD 경로 |
| `MARIONETTE_MAX_EMULATORS` | `2` | `0` | 선택, `0`으로 생성 금지, 최대 `32` | 시작 시 관리 에뮬레이터 수 상한 |
| `MARIONETTE_ICE_SERVERS` | `stun:stun.l.google.com:19302` | `stun:stun.example.com:3478` 또는 `none` | 선택 | 시작 후 새 직접 ADB 연결 |
| `MARIONETTE_ICE_USERNAME` | 없음 | `farm-agent` | TURN 사용 시 필수 | 시작 후 새 직접 ADB 연결 |
| `MARIONETTE_ICE_PASSWORD` | 없음 | TURN 비밀값 | TURN 사용 시 필수 | 시작 후 새 직접 ADB 연결 |
| `MARIONETTE_MAX_UPLOAD_BYTES` | `1073741824` (1 GiB) | `536870912` | 선택 | 시작 후 파일 업로드 및 APK 설치 요청 |
| `MARIONETTE_MAX_DOWNLOAD_BYTES` | `536870912` (512 MiB) | `268435456` | 선택 | 시작 후 기기 파일 다운로드 요청 |
| `MARIONETTE_DISCOVERY_INTERVAL` | `2s` | `5s` | 선택, `1s`–`1m` | 시작 시 ADB 기기 검색 주기 |
| `MARIONETTE_WIPE_KEEP_PACKAGES` | 빈 목록 | `com.example.kiosk,com.example.tools` | 선택 | 반납 정리 때 유지할 Android 앱 패키지 이름, 쉼표로 구분 |
| `MARIONETTE_WIPE_TIMEOUT` | `3m` | `5m` | 선택, `1s`–`1h` | 반납 정리 전체 제한 시간 |
| `JAVA_HOME` | SDK 옆 JDK 추론 | `/opt/jdk` | 선택 | 시작 시 AVD 도구용 Java 경로 |
| `ADB_SERVER_SOCKET` | 없음 | `localfilesystem:/run/adb.sock` | 선택 | 직접 ADB 연결 시 ADB 소켓 |
| `ANDROID_ADB_SERVER_PORT` | `5037` | `5038` | 선택 | `ADB_SERVER_SOCKET` 미설정 시 직접 ADB 연결 포트 |

ICE 서버를 쉼표로 구분합니다. TURN 주소 예시는 `turns:turn.example.com:5349?transport=tcp`이며 에이전트의 모든 TURN 서버에서 같은 자격 증명을 사용합니다. 전송 한도는 1바이트에서 1TiB 사이 정수로 설정합니다. 역방향 프록시에도 별도 업로드 한도가 있으면 함께 조정하세요.

기기 허용 목록과 동시 전송·임시 저장 한도는 아래 표를 참고하세요.

완료되지 않은 반납 정리 상태는 `MARIONETTE_STATE_DIR`의 `wipe-required.json`에 보존됩니다. 에이전트 재시작 후 발견된 해당 기기는 관리자가 정리를 다시 실행하거나 복귀시킬 때까지 배정할 수 없습니다.

## 공통 설정

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_LOG_LEVEL` | `info` | `debug`, `warn`, `error` | 선택, 네 실행 파일 | 시작 시 로그 필터 |
| `MARIONETTE_LOG_FORMAT` | `text` | `json` | 선택, 네 실행 파일 | 시작 시 로그 출력 형식 |
| `MARIONETTE_LEASE_DEFAULT_TTL` | `5m` | `10m` | 선택, 백엔드와 에이전트 | 시작 후 기간을 생략한 점유·갱신 및 기기 인계 |
| `MARIONETTE_LEASE_MAX_TTL` | `1h` | `45m` | 선택, 백엔드와 에이전트 | 시작 후 점유 및 갱신 요청의 최대 기간 |

기간은 `30s`, `5m`, `8h`처럼 초 단위까지 입력합니다. 점유 기본값과 최대값은 `30s`–`24h`이고 기본값은 최대값 이하여야 합니다. 기기 점유 설정은 백엔드와 연결되는 에이전트에 동일하게 적용하세요. 새 기본값은 이후의 요청에 적용됩니다. 현재 웹 콘솔이 직접 보내는 점유·갱신 요청과 CLI 기본 요청은 `300`초를 지정하므로 해당 클라이언트를 쓰면 최대값을 `5m` 이상으로 두세요. Kafka 작업에는 최소 10초의 실행 시간과 30초의 여유가 필요하므로 Kafka를 활성화하면 점유 최대값은 `40s` 이상이어야 합니다.

## 기존 설정의 위치

| 항목 | 이전 구성 |
| --- | --- |
| 공개 주소, 웹 오리진, OIDC, 관리자 | `cmd/marionette-backend/main.go`에서 환경 변수를 읽었습니다. OIDC 콜백은 항상 별도로 설정했고, 공개 주소와 관리자 목록은 선택 사항이었습니다. |
| 수신 주소, 상태 파일, 웹 파일과 설치 파일 경로 | 각 `cmd/marionette-{backend,web,agent}/main.go`의 플래그 기본값 또는 `internal/backend/downloads.go`의 설치 파일 자동 탐색을 사용했습니다. |
| 브라우저 세션과 MCP OAuth 수명 | `internal/auth/auth.go`의 세션 기본값은 `8h`, `internal/auth/oauth.go`의 액세스 토큰·갱신 토큰·코드 수명은 각각 `1h`, `720h`, `5m`였습니다. |
| 점유 시간 | `internal/host/server.go` 및 `internal/host/handoff.go`에서 기본 `300`초·최대 `3600`초를 사용했고, `internal/backend/automation.go`에도 같은 범위가 있었습니다. |
| 기기 전송과 검색 | `internal/host/files.go`의 업로드·다운로드 상한은 `1 GiB`와 `512 MiB`였고, `internal/host/server.go`의 검색 주기는 `2s`였습니다. |
| ICE, Kafka, 러너 | ICE 목록과 Kafka 접속 정보, 러너 토큰은 이미 환경 변수로 읽었습니다. `cmd/marionette-runner/main.go`의 소비자 그룹은 플래그 기본값이었고, TURN 비밀번호용 환경 변수는 없었습니다. |
| 로그 | 네 실행 파일 모두 Go `slog`의 기본 출력을 사용했습니다. |

## 에이전트 기기 허용 목록과 전송 한도

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_DEVICE_ALLOW` | 모든 실물 및 이 에이전트가 관리하는 에뮬레이터 | `physical:*,managed-only,emulator-5580` | 선택, 시리얼과 예약값을 쉼표로 구분 | 시작 시 기기 검색, 상시 대기 및 충전 중 화면 켜두기 |
| `MARIONETTE_MAX_LEASE_TRANSFERS` | `2` | `4` | 선택, `1`–`128` | 점유당 동시 파일 업로드·다운로드 및 APK 설치 |
| `MARIONETTE_MAX_AGENT_TRANSFERS` | `8` | `16` | 선택, `1`–`128` | 에이전트당 동시 파일 업로드·다운로드 및 APK 설치 |
| `MARIONETTE_MAX_STAGING_BYTES` | `2147483648` (2 GiB) | `4294967296` | 선택, `1`바이트–`1`TiB | 에이전트 호스트의 전체 APK 임시 저장량. 업로드가 끝나기 전에도 선언한 바이트를 예약 |

## 백엔드 동영상 연결과 작업 보관

| 환경 변수 | 기본값 | 예시 | 필요 여부 | 적용 시점 |
| --- | --- | --- | --- | --- |
| `MARIONETTE_VIDEO_MAX_GLOBAL` | `128` | `64` | 선택, 1–1,048,576 | 시작 후 백엔드 전체의 동시 동영상 및 관리자 미리보기 WebSocket 수 |
| `MARIONETTE_VIDEO_MAX_PER_ACCOUNT` | `4` | `2` | 선택, 1–1,048,576 | 시작 후 계정 또는 점유 1건당 동시 동영상 WebSocket 수 |
| `MARIONETTE_JOB_RETENTION` | `168h` | `336h` | 선택, 양의 기간 | Kafka 작업 사용 시 종료 작업과 결과 파일 보관 기간 |
| `MARIONETTE_JOB_ARTIFACT_MAX_BYTES_PER_ACCOUNT` | `268435456` (256 MiB) | `536870912` | 선택, 1바이트–1 TiB | Kafka 작업 사용 시 작업 결과 파일의 계정당 바이트 한도 |
