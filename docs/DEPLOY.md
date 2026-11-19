# Docker Compose로 서버 배포

백엔드, 웹 콘솔, Caddy를 한 호스트에서 실행합니다. 기본 구성은 `https://devices.example.com` 같은 공개 HTTPS 오리진 하나를 사용합니다. Caddy만 외부 포트를 열며 백엔드와 웹은 컨테이너 네트워크에서 각각 `8080`, `3000`으로 수신합니다. USB와 Android 도구는 기기가 연결된 에이전트 PC에 둡니다.

## 1. DNS와 OIDC 준비

- Docker Engine과 Docker Compose v2.30 이상을 설치하고 저장소 루트에서 명령을 실행합니다. 서버 이미지는 Linux amd64와 arm64를 지원합니다.
- 공개 도메인의 DNS `A` 레코드를 서버 주소로 연결합니다. `AAAA`를 사용하는 경우 IPv6에서도 서버에 접속할 수 있어야 합니다. 방화벽과 NAT에서 TCP `80`, `443`을 Caddy로 연결합니다. Caddy가 자동으로 HTTPS 인증서를 발급·갱신하고 HTTP를 HTTPS로 리디렉션합니다.
- 회사 OIDC에서 client secret을 사용하는 웹 클라이언트를 등록합니다. 리디렉션 URI에는 `https://devices.example.com/auth/callback`처럼 **공개 HTTPS 주소와 `/auth/callback`이 정확히 일치하는 값**을 등록합니다.

## 2. 환경 설정과 실행

```sh
export MARIONETTE_DOMAIN=devices.example.com
unset MARIONETTE_API_URL  # 단일 오리진에서 비워 둡니다.
cp -n deploy/docker/backend.env.example deploy/docker/backend.env
chmod 600 deploy/docker/backend.env
mkdir -p build
```

`deploy/docker/backend.env`에서 `MARIONETTE_OIDC_ISSUER`, `MARIONETTE_OIDC_CLIENT_ID`, `MARIONETTE_OIDC_CLIENT_SECRET`을 회사 OIDC 값으로 바꿉니다. `MARIONETTE_PUBLIC_URL`에는 `https://devices.example.com`, `MARIONETTE_OIDC_REDIRECT_URL`에는 `https://devices.example.com/auth/callback`을 지정합니다. `MARIONETTE_ADMIN_EMAILS`에는 OIDC가 검증한 관리자 이메일을, `MARIONETTE_ADMIN_SUBJECTS`에는 관리자 계정의 정확한 `sub`를 쉼표로 구분해 넣을 수 있습니다. 두 목록 중 최소 하나를 채웁니다. 기본 구성에서는 `MARIONETTE_FRONTEND_ORIGINS`를 비웁니다.

Compose는 `backend.env`의 값을 `KEY=value` 형식 그대로 읽습니다. 값에 따옴표를 더하거나 이 파일을 셸에서 `source`하지 않습니다. 비밀 파일은 배포 호스트에서 권한을 제한해 관리합니다. `MARIONETTE_DOMAIN`은 컨테이너 환경 파일과 별개로 Compose를 실행하는 셸에 설정합니다. 호스트 이름만 입력합니다. 전체 변수와 우선순위는 [환경 변수 안내](CONFIG.md)에 있습니다.

```sh
docker compose config -q
docker compose up -d --build
docker compose ps
docker compose logs --tail=100 backend web proxy
curl -fsS "https://$MARIONETTE_DOMAIN/api/session"
```

Dockerfile 하나가 Go `1.26.11`로 `marionette-backend`, `marionette-web`, `marionette-runner`를 만들어 Alpine 런타임 이미지 하나에 담습니다. 세 서비스는 UID/GID `10001:10001`로 실행하고 루트 파일시스템을 읽기 전용으로 사용합니다. Compose의 수신 주소와 데이터 경로는 이미지 및 서비스 명령의 플래그로 고정합니다. 서버 이미지 빌드에는 Go나 Android 도구를 호스트에 따로 설치할 필요가 없습니다.

Caddy는 `/api/*`, `/auth/*`, `/oauth/*`, `/.well-known/*`, `/mcp`, `/downloads/*`, `/install/*`, `/internal/agents/*`, `/internal/runners/*`를 백엔드로 보냅니다. 나머지 경로는 웹에서 처리합니다. 원래 `Host`와 WebSocket 업그레이드가 백엔드에 전달됩니다. 영상·제어·ADB WebSocket에는 프록시 스트림 제한 시간을 걸지 않습니다. MCP 응답은 바로 전송합니다. 프록시의 요청 본문 한도는 `2 GiB`로, 에이전트의 기본 `1 GiB` 업로드에 필요한 멀티파트 공간을 포함합니다.

기본 구성에서는 웹이 현재 페이지의 오리진을 API와 WebSocket에 사용합니다. 웹을 별도 오리진에 두려면 웹 서비스 앞에 그 도메인의 HTTPS 사이트를 추가하거나 웹을 별도 호스트에 배포합니다. Compose의 `MARIONETTE_API_URL`과 에이전트 주소에는 백엔드의 HTTPS 오리진을 지정하고, 백엔드 `MARIONETTE_FRONTEND_ORIGINS`에는 허용할 웹 오리진을 지정합니다. 목록의 첫 오리진이 로그인 뒤 복귀 주소입니다. OIDC 콜백은 백엔드 공개 주소를 사용하고, 프록시는 해당 주소의 `Host`를 보존합니다. 백엔드는 설정한 공개 주소를 기준으로 `Secure` 쿠키를 발급하고, 변경 요청에는 CSRF 토큰을 확인합니다. 브라우저의 교차 사이트 쿠키 제한을 피하려면 같은 사이트의 도메인을 사용합니다.

## 3. 첫 관리자 로그인과 Ubuntu 에이전트 연결

1. 공개 HTTPS 주소를 열고 회사 OIDC에서 설정한 관리자 계정으로 로그인합니다. 관리자 메뉴가 보이지 않으면 이메일 검증 여부와 설정에 넣은 이메일 또는 `sub`가 일치하는지 확인합니다.
2. 별도 빌드 호스트에서 `make tools`와 `make agent-package-linux`를 실행하거나 같은 버전의 배포 패키지를 준비합니다. `marionette-agent-linux-amd64.tar.gz`, `marionette-agent-linux-arm64.tar.gz`, `marionette-agent-linux-SHA256SUMS`를 배포 호스트의 `build/`에 놓습니다. 다른 호스트 경로를 쓸 때는 Compose 실행 전에 `MARIONETTE_AGENT_PACKAGES_DIR`을 설정합니다. 디렉터리와 파일은 컨테이너 UID `10001`이 읽을 수 있어야 합니다. 설치 스크립트는 이미지에 들어 있으며 패키지는 읽기 전용 마운트에서 제공합니다. 빈 `build/`로 서버를 시작할 수 있으며 해당 패키지를 넣은 뒤 다운로드가 가능합니다.
3. 대시보드의 **에이전트 운영 → 에이전트 연결**에서 PC ID를 등록하고 발급받은 토큰을 보관합니다. 등록한 PC의 다음 명령에서 도메인과 ID를 바꿉니다.

   ```bash
   export MARIONETTE_BACKEND_URL=https://devices.example.com
   curl -fsSL "$MARIONETTE_BACKEND_URL/install/ubuntu.sh" | bash -s -- --id ubuntu-01
   ```

   프롬프트에 PC 토큰을 붙여 넣고 설치 뒤 대시보드에서 연결과 기기 목록을 확인합니다. USB 권한과 에뮬레이터 설치는 [Ubuntu 에이전트 안내](UBUNTU_AGENT.md)에 있습니다.

## 4. 선택 사항인 Kafka와 러너

작업 큐를 쓸 때만 `deploy/docker/jobs.env.example`을 `deploy/docker/jobs.env`로 복사하고 `chmod 600`을 적용합니다. `openssl rand -hex 32`로 만든 값을 `MARIONETTE_RUNNER_TOKEN`에 넣습니다. 백엔드와 러너는 이 파일을 공유합니다. 내장 예제는 외부 포트가 없는 단일 노드 KRaft 브로커이며 브로커 장애를 견디는 복제 구성은 없습니다.

```sh
docker compose --profile kafka up -d --wait kafka
docker compose exec kafka /opt/kafka/bin/kafka-topics.sh \
  --bootstrap-server kafka:9092 --create --if-not-exists \
  --topic marionette.jobs --partitions 1 --replication-factor 1
docker compose --profile kafka --profile runner up -d --build
```

다른 토픽을 설정하면 위 명령의 이름도 바꿉니다. 토픽은 자동 생성하지 않습니다. 러너는 Caddy를 통해 `https://$MARIONETTE_DOMAIN`의 백엔드에 접속하므로 공개 인증서 발급이 먼저 끝나야 합니다. 별도 Kafka를 사용하면 `jobs.env`에서 브로커와 TLS·인증을 지정하고 `--profile runner`만 켭니다. 같은 토픽을 처리하는 러너는 같은 소비자 그룹을 사용합니다.

## 5. 백업, 업데이트, 로그

`backend_data` 볼륨의 `/data/state`에 `tokens.json`, `users.json`, `agents.json`, `oauth.json`, `jobs.json`과 `artifacts/`가 저장됩니다. 이 JSON 파일에는 백엔드 한 프로세스만 쓰도록 **백엔드 복제본을 1개**로 유지합니다. 재시작하면 브라우저 세션은 종료됩니다.

작업을 마친 유지보수 시간에 백엔드를 정지하고 볼륨을 백업합니다. Kafka를 쓰면 실행 중인 작업을 먼저 마무리하고 러너도 정지합니다.

```sh
docker compose stop backend
(umask 077 && docker compose run --rm --no-deps -T --entrypoint tar backend \
  -czf - -C /data/state . > "marionette-state-$(date +%Y%m%d-%H%M%S).tar.gz")
docker compose start backend
```

복원할 때는 정지된 백엔드에서 빈 대상 볼륨을 준비한 뒤 `docker compose run --rm --no-deps -T --entrypoint tar backend -xzf - -C /data/state < 백업파일.tar.gz`를 실행합니다. 설정 파일과 에이전트 패키지는 별도로 보관합니다. `caddy_data`에는 인증서와 키, `kafka_data`에는 브로커 데이터가 있으므로 사용 중인 경우 함께 보존합니다.

서버와 패키지를 같은 릴리스로 갱신하고 `docker compose up -d --build`를 다시 실행합니다. Kafka와 러너를 운영 중이면 두 프로필을 함께 지정합니다. 설정 파일을 바꾼 뒤에도 `up -d`를 실행해 변경된 컨테이너를 재생성합니다. 운영 데이터 보존을 위해 `docker compose down`에 `-v`를 붙이지 않습니다.

```sh
docker compose ps
docker compose logs --tail=100 -f backend web proxy
```

백엔드 상태 검사는 공개 `Host`로 `GET /api/session`에 요청해 `"configured":true`를 확인합니다. 웹 상태 검사는 `GET /`를 사용합니다. 백엔드의 `/healthz`와 `/readyz`는 컨테이너 네트워크에서 직접 확인할 수 있습니다. 운영 확인에는 회사 SSO 로그인, 기기 연결, Kafka 작업도 각각 확인합니다. 모든 서비스는 표준 출력과 표준 오류를 Docker `json-file`로 수집하며, 컨테이너마다 `10 MB`씩 최대 3개 파일을 보관합니다.

## 로컬 검증용 Dex

공개 도메인 없이 로그인 흐름을 확인할 때는 별도 Compose 프로젝트에서 아래 예제를 실행합니다. HTTP와 공개된 예제 비밀번호를 사용하므로 운영에서는 회사 OIDC를 사용합니다. 포트 `18089`와 `18556`은 호스트의 루프백에만 공개합니다.

```sh
mkdir -p build
export MARIONETTE_DOMAIN=127.0.0.1
export MARIONETTE_BACKEND_ENV_FILE=./deploy/docker/backend.env.example
export MARIONETTE_JOBS_ENV_FILE=/dev/null
export MARIONETTE_API_URL=
docker compose -p marionette-local -f docker-compose.yml \
  -f deploy/docker/compose.dex.yml --profile dex up -d --build
curl -fsS http://127.0.0.1:18089/api/session
```

`http://127.0.0.1:18089`에서 `admin@example.com`과 `password`로 로그인한 뒤 Dex의 접근 승인 화면을 통과합니다. 검증이 끝나면 아래 명령으로 **이 로컬 프로젝트의** 컨테이너와 볼륨을 지웁니다.

```sh
docker compose -p marionette-local -f docker-compose.yml \
  -f deploy/docker/compose.dex.yml --profile dex down -v
```
