# 서버 프로세스 자동 시작

백엔드, 웹 콘솔, HTTPS 프록시를 컨테이너로 운영하려면 [Docker Compose 배포 안내](../docs/DEPLOY.md)를 참고하세요.

Android USB 에이전트는 [에이전트 설치 안내](../docs/AGENT_INSTALL.md), Ubuntu 에이전트는 [Ubuntu 안내](../docs/UBUNTU_AGENT.md)를 사용합니다. 여기서는 별도 프로세스인 백엔드, 프론트엔드, 선택적 Kafka 러너를 다룹니다. 아래 명령은 배포 담당자가 검토한 뒤 해당 호스트에서 실행하는 예시입니다. 파일 생성만으로 서비스가 등록되지는 않습니다.

## 설치 레이아웃과 기존 데이터 보존

배포 경로는 고정하며 아래 파일을 같은 버전으로 준비합니다. 빌드는 개발·빌드 호스트에서 `make backend frontend runner`로 수행합니다. 대상 OS·CPU용 바이너리가 필요하며 소스의 `go run`을 서비스에서 실행하지 않습니다. Go와 Android SDK는 이 서버 레이아웃 실행에 필요하지 않습니다.

```text
~/.local/share/marionette/
  build/marionette-backend
  build/marionette-web
  build/marionette-runner       # Kafka 작업을 사용할 때
  scripts/start-service.sh
  scripts/render-launchd.py    # macOS 설정 생성에만 Python 3 필요
  web/                        # 저장소의 정적 파일 전체
  deploy/                     # 서비스 및 환경 설정 예제
  .env.backend
  .env.web
  .env.runner                 # 선택
  .marionette/                # 기본 영구 상태 경로
```

기존 서버를 옮기면 기존 `.marionette`를 보존하거나 `.env.backend`의 `MARIONETTE_STATE_DIR`에 **기존 디렉터리의 절대 경로**를 지정합니다. `tokens.json`(PAT 해시), `agents.json`(에이전트 등록 해시), `jobs.json`(작업 상태)을 함께 유지하며 동일 경로에 두 백엔드를 동시에 실행하지 않습니다. 기존 `--token-file`, `--agent-file`, `--job-file`이 서로 다른 경로라면 시작 스크립트 실행 인자 또는 서비스의 `ProgramArguments`/`ExecStart`에 각 플래그를 그대로 추가합니다. 실행 파일 업데이트가 상태 초기화·토큰 재발급을 요구하지 않습니다.

각 프로세스의 설정 파일은 Bash로 읽습니다. 실제 비밀값은 로컬 설정 파일에만 넣고 값에 셸 특수문자가 있으면 따옴표로 감쌉니다. 예제 파일을 기존 설정 위에 덮어쓰지 않습니다.

```sh
cd "$HOME/.local/share/marionette"
cp -n deploy/backend.env.example .env.backend
cp -n deploy/web.env.example .env.web
chmod 600 .env.backend .env.web
# 편집기로 기존 OIDC 설정·관리자 목록·공개 URL을 입력합니다.
bash scripts/start-service.sh backend
# 다른 터미널에서:
bash scripts/start-service.sh web
```

기존 `MARIONETTE_AGENT_TOKENS` 사전 등록을 사용했다면 해당 JSON 값도 `.env.backend`에 그대로 보존합니다. 웹에서 등록한 PC는 기존 `agents.json`으로 복원합니다. 기본 수신은 백엔드 `127.0.0.1:8080`, 웹 `127.0.0.1:3000`입니다. 외부 접속에는 HTTPS 역방향 프록시를 두고 `/auth/*`, `/api/*`, `/internal/agents/*`의 WebSocket Upgrade를 백엔드로 전달합니다. 백엔드 OIDC 콜백·프론트엔드 API 주소·공개 오리진을 실제 도메인과 일치시킵니다. `/internal/runners/*`도 러너가 접속하는 백엔드 경로로 전달합니다.

Kafka 작업을 사용하면 `.env.runner`를 예제에서 만들고 백엔드와 같은 broker·topic·TLS·SASL 설정 및 **기존의 별도 러너 토큰**을 넣습니다. `.env.backend`에도 같은 Kafka 설정과 토큰이 필요합니다. Kafka 자체는 별도 운영 서비스로 준비하고 기존 broker 저장소·topic·consumer group을 보존합니다. `deploy/kafka-local.properties`는 로컬 예제이며 서버 자동 시작 템플릿에 포함되지 않습니다. 러너를 여러 대 운영할 때는 공통 consumer group을 사용합니다.

## Ubuntu 사용자 systemd

고정 경로가 다르면 `deploy/marionette@.service`의 `WorkingDirectory`와 `ExecStart`를 함께 바꿉니다. 수동으로 실행한 해당 프로세스를 정상 종료한 뒤 같은 일반 사용자로 등록합니다.

```sh
mkdir -p "$HOME/.config/systemd/user"
cp -n deploy/marionette@.service "$HOME/.config/systemd/user/"
systemd-analyze --user verify "$HOME/.config/systemd/user/marionette@.service"
systemctl --user daemon-reload
systemctl --user enable --now marionette@backend.service marionette@web.service
# 로그인하지 않아도 부팅 시 사용자 서비스가 시작되도록 허용합니다.
sudo loginctl enable-linger "$(id -un)"
journalctl --user -u marionette@backend.service -u marionette@web.service -f
# Kafka가 준비된 배포에서만:
systemctl --user enable --now marionette@runner.service
```

등록 전 `cp -n`으로 보존된 기존 unit도 확인합니다. 이 템플릿은 재시작을 5초 간격으로 계속 시도하므로 네트워크·OIDC·Kafka가 늦게 준비되어도 복구를 시도합니다. 실행 상태는 애플리케이션 준비 완료와 같지 않습니다. `.env` 변경 후 `systemctl --user restart marionette@backend.service`처럼 해당 인스턴스를 재시작합니다. 정지는 `systemctl --user disable --now marionette@backend.service`를 사용하고 웹·러너도 필요에 따라 각각 지정합니다. 이 절차는 개인 계정의 다른 서비스에 쓰일 수 있는 linger를 해제하지 않습니다.

## macOS 사용자 launchd

macOS 구성은 사용자 **로그인 후** 시작합니다. FileVault 잠금 해제 전·로그인 전 부팅 서비스를 제공하지 않습니다. 아래 생성기는 plist에 비밀값을 넣지 않고 시작 스크립트와 로그의 절대 경로를 기록합니다. 기존 출력 파일은 거부합니다.

```sh
cd "$HOME/.local/share/marionette"
python3 scripts/render-launchd.py backend --install-dir "$PWD" --output ./io.marionette.backend.plist
python3 scripts/render-launchd.py web --install-dir "$PWD" --output ./io.marionette.web.plist
plutil -lint ./io.marionette.backend.plist ./io.marionette.web.plist
plutil -p ./io.marionette.backend.plist
mkdir -p "$HOME/Library/LaunchAgents"
cp -n ./io.marionette.backend.plist ./io.marionette.web.plist "$HOME/Library/LaunchAgents/"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/io.marionette.backend.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/io.marionette.web.plist"
launchctl print "gui/$(id -u)/io.marionette.backend"
tail -f .marionette/logs/backend.log .marionette/logs/web.log
```

러너도 필요하면 같은 생성기의 `runner` 구성으로 만들고 등록합니다. 로그는 설치 경로의 `.marionette/logs/<component>.log`에 쌓이므로 장기 운영에는 로그 순환을 추가합니다. 수정·정지 시 `launchctl bootout "gui/$(id -u)/io.marionette.backend"`로 먼저 정상 종료합니다. 검토한 plist를 교체한 뒤 다시 `bootstrap`하며, 자동 실행을 제거할 때는 해당 plist를 `~/Library/LaunchAgents`에서 제거합니다. `KeepAlive` 구성은 프로세스를 직접 종료하면 다시 시작하므로 서비스 관리자를 통해 정지합니다.

## 배포 확인

프록시를 통한 프론트엔드 로딩, OIDC 로그인, 기존 PAT 사용, 같은 ID의 에이전트 재접속과 기존 등록 목록, 기기 목록을 확인합니다. Kafka 사용 시 작업 저장소와 기존 queue/consumer group 유지도 확인합니다. 실제 재부팅 검증은 점유 중인 기기와 실행 작업이 없는 유지보수 시간에 수행합니다. Linux 바이너리 교차 빌드, plist 문법 검사, 컨테이너 내 unit 검사만으로 Ubuntu의 systemd 부팅·USB·KVM 또는 Android Studio의 GUI 실행·디버깅을 검증했다고 판단하지 않습니다.

서비스 의미는 [systemd.service](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html), 부팅 후 사용자 서비스 유지 조건은 [loginctl enable-linger](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html)를 참고합니다. launchd 항목은 macOS의 `man launchd.plist`와 `man launchctl`에서 확인할 수 있습니다.
