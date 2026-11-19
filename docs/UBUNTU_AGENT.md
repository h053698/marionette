# Ubuntu에서 상위 서버에 연결

## 한 번에 설치

Ubuntu 또는 Debian 데스크톱에서 일반 사용자로 실행합니다. 웹 콘솔의 **에이전트 운영 → 에이전트 연결**에서 이 PC의 ID와 토큰을 발급받으세요. 백엔드 주소에는 상위 백엔드의 HTTPS 오리진을 사용합니다.

```bash
export MARIONETTE_BACKEND_URL=https://api.example.com
curl -fsSL "$MARIONETTE_BACKEND_URL/install/ubuntu.sh" | bash -s -- --id ubuntu-01
```

토큰은 프롬프트에 붙여 넣으세요. 입력값은 화면에 표시되지 않습니다. 스크립트가 PC 아키텍처에 맞는 패키지를 내려받고 SHA-256을 검사합니다. `curl`이 없거나 이미 패키지를 받았다면 패키지를 푼 디렉터리에서 실행하세요.

```bash
bash setup-ubuntu-agent.sh --backend https://api.example.com --id ubuntu-01
```

- **실물 기기**가 기본입니다. USB udev 규칙과 ADB를 설치합니다. 그룹 변경 후 다시 로그인하고, 휴대폰의 USB 디버깅을 켠 뒤 연결해 컴퓨터 인증을 허용하세요. 사용자 서비스에 이전 그룹 권한이 남으면 재부팅하세요.
- **에뮬레이터**는 위 명령 끝에 `--mode emulator --emulators 2`를 붙입니다. amd64에서 전용 Android SDK, Android 34 Google APIs 이미지, Temurin JDK 17을 설치합니다. arm64에서는 실물 기기용으로 설치합니다. 기존 Android Studio SDK와 별도로 설치하며 라이선스 자동 동의 전에 확인합니다. `--emulators`는 상한입니다. 실제 실행 수와 이미지는 대시보드의 **운영 설정**에서 지정합니다.
- `--tags lab,android`로 태그를 지정하고 `--dry-run`으로 작업 계획을 볼 수 있습니다. 자동화에서는 `MARIONETTE_BACKEND_URL`, `MARIONETTE_AGENT_ID`, `MARIONETTE_AGENT_TOKEN`을 환경 변수로 전달하고 `--yes`를 붙입니다. `--yes`에서 sudo도 입력을 받지 않으므로 해당 계정의 sudo 인증을 먼저 준비하세요.

설치 후 **에이전트 운영**에서 연결 상태와 기기 목록을 확인하세요. `systemctl --user status marionette-agent.service`로 상태를 보고, `journalctl --user -u marionette-agent.service -f`로 로그를 봅니다. 부팅 직후 자동 실행하려면 설치 중 linger를 허용하세요.

업그레이드는 같은 URL로 다시 실행하거나 새로 푼 패키지에서 실행합니다. 기존 상태와 변경하지 않은 `.env` 값이 유지됩니다. 에뮬레이터 상한을 낮추려면 대시보드의 실행 수를 먼저 줄이세요.

```bash
bash "$HOME/.local/share/marionette-agent/setup-ubuntu-agent.sh" --uninstall
```

제거하면 사용자 서비스, 설정 파일과 설치 폴더 안의 상태가 삭제됩니다. 전용 SDK와 JDK도 삭제하려면 `--purge`를 붙이세요.

## 수동 설치

휴대폰을 USB로 연결하는 Ubuntu 컴퓨터에 PC 에이전트만 설치합니다. Go, JDK, 프론트엔드, 백엔드를 이 컴퓨터에 설치할 필요는 없습니다. 에이전트가 상위 중계 백엔드로 접속하므로 Ubuntu의 수신 포트를 열지 않습니다.

### 1. 패키지 선택과 USB 준비

`uname -m`이 `x86_64`이면 `marionette-agent-linux-amd64.tar.gz`, `aarch64`이면 `marionette-agent-linux-arm64.tar.gz`를 사용합니다. 패키지를 Ubuntu 컴퓨터로 복사합니다.

```sh
sudo apt-get update
sudo apt-get install -y adb android-sdk-platform-tools-common ca-certificates
sudo usermod -aG plugdev "$(id -un)"
```

그룹 변경 후 로그아웃하고 다시 로그인합니다. 휴대폰에서 USB 디버깅을 켜고 USB를 연결한 뒤 컴퓨터 인증을 허용합니다.

```sh
adb devices -l
```

해당 휴대폰의 상태가 `device`여야 합니다. `unauthorized`면 휴대폰에서 인증을 허용하고, `no permissions`면 그룹 재로그인과 udev 규칙 설치를 확인한 후 USB를 다시 연결합니다. ADB와 에이전트는 같은 일반 사용자로 실행합니다.

### 에뮬레이터를 함께 운영하는 Ubuntu

실물 기기 전용이면 이 절차는 필요하지 않습니다. 에뮬레이터는 SDK Emulator·Command-line Tools·호스트 CPU에 맞는 시스템 이미지와 JDK가 추가로 필요합니다. Linux amd64에는 `x86_64` 이미지를 사용합니다. **Linux arm64 에이전트 패키지 지원이 Linux arm64용 Android Studio·Emulator 지원을 뜻하지는 않습니다.** 해당 호스트에서 공식 Emulator가 제공되고 실행 가능한지 먼저 확인하고, 지원 도구가 없으면 실물 전용으로 운영합니다.

일반 Intel·AMD Ubuntu에서는 BIOS/UEFI의 CPU 가상화를 활성화하고 KVM 접근 권한을 확인합니다. 아래 권한 변경은 해당 Ubuntu 호스트에서 관리자가 수행합니다.

```sh
sudo apt-get install -y cpu-checker qemu-kvm
sudo usermod -aG kvm "$(id -un)"
# 그룹 변경 후 로그아웃·로그인한 뒤 실행합니다.
id
kvm-ok
test -r /dev/kvm && test -w /dev/kvm
/absolute/android-sdk/emulator/emulator -accel-check
```

가상 머신이나 컨테이너에서는 `/dev/kvm` 접근과 중첩 가상화를 호스트가 제공해야 합니다. 컨테이너 실행이나 Linux 바이너리 교차 빌드 성공만으로 Ubuntu의 USB·udev·KVM·부팅 자동 시작이 검증되지는 않습니다. 자세한 조건은 [Android Emulator 공식 가속 안내](https://developer.android.com/studio/run/emulator-acceleration#vm-linux)를 확인합니다.

패키지 `.env`에 서비스가 사용할 절대 경로를 추가합니다. 사용자 로그인 셸의 PATH·JAVA_HOME은 systemd 서비스에 자동 전달되지 않습니다.

```sh
MARIONETTE_ADB=/absolute/android-sdk/platform-tools/adb
MARIONETTE_ANDROID_SDK=/absolute/android-sdk
JAVA_HOME=/absolute/jdk
# 기존 운영 상태 경로를 그대로 유지합니다.
MARIONETTE_STATE_DIR="$HOME/.local/share/marionette-agent/.marionette/agent-ubuntu-01"
MARIONETTE_MAX_EMULATORS=2
```

Android Studio에서 SDK를 준비했다면 SDK Location을 위 값과 맞춥니다. 원격 Studio 사용은 [PC 에이전트 설치 안내](AGENT_INSTALL.md#에뮬레이터용-sdk-준비)와 [원격 ADB 안내](AUTOMATION.md)를 참고합니다.

### 2. 설치와 상위 서버 등록

아래는 Intel·AMD Ubuntu 예시입니다. ARM이면 패키지 이름의 `amd64`를 `arm64`로 바꿉니다.

```sh
mkdir -p "$HOME/.local/share/marionette-agent"
tar -xzf marionette-agent-linux-amd64.tar.gz \
  --strip-components=1 -C "$HOME/.local/share/marionette-agent"
cd "$HOME/.local/share/marionette-agent"
cp -n .env.example .env
chmod 600 .env
nano .env
```

`.env`에서 세 값을 실제 환경에 맞게 설정합니다. URL은 **프론트엔드 주소가 아닌 상위 백엔드의 HTTPS 오리진**입니다.

```sh
MARIONETTE_BACKEND_URL=https://api.example.com
MARIONETTE_AGENT_ID=ubuntu-01
MARIONETTE_AGENT_TOKEN=replace-with-the-token-registered-on-the-backend
```

관리자는 웹 콘솔의 **에이전트 운영 → 에이전트 연결**에서 PC별 ID와 이름을 등록하고, 한 번 표시되는 토큰을 이 PC의 `.env`에 넣습니다. 백엔드를 재시작할 필요는 없습니다.

환경 변수로 미리 등록하는 방식도 지원합니다. 이 경우 `openssl rand -hex 32` 등으로 토큰을 생성하고, 상위 백엔드의 `MARIONETTE_AGENT_TOKENS`에 기존 PC 항목을 유지하면서 ID와 토큰을 추가한 뒤 백엔드를 재시작합니다.

```sh
MARIONETTE_AGENT_TOKENS='{"ubuntu-01":"replace-with-the-same-token-as-the-agent"}'
```

한 ID에는 동시에 한 에이전트만 연결할 수 있습니다. PC마다 ID와 토큰을 따로 등록합니다.

상위 서버의 HTTPS 프록시는 `/internal/agents/ubuntu-01/connect`의 WebSocket Upgrade를 백엔드로 전달해야 합니다. Ubuntu에서 상위 서버의 HTTPS 포트(기본 443)로 나가는 연결이 가능해야 합니다. 에이전트 설정에는 `https://`를 입력하며 WebSocket 주소 변환은 프로그램에서 처리합니다.

연결한 뒤에는 대시보드의 **운영 설정**에서 실물 전용·에뮬레이터 전용·혼합을 선택합니다. 에뮬레이터 생성에는 추가 SDK·JDK가 필요합니다. 설치 조건과 상시 대기 설정은 [PC 에이전트 운영 안내](AGENT_INSTALL.md#대시보드에서-pc-운영-방식-정하기)를 참고하세요.

### 3. 바로 연결

```sh
cd "$HOME/.local/share/marionette-agent"
bash start-agent.sh --adb /usr/bin/adb
```

`agent connected to relay` 로그가 나오면 상위 서버에 연결된 것입니다. 상위 서버의 웹 콘솔에서 이 PC의 휴대폰을 확인합니다. 연결이 끊기면 1–30초 간격으로 재접속합니다. 터미널 실행은 `Ctrl+C`로 종료합니다.

상위 서버가 HTTPS 없이 HTTP만 제공한다면 SSH 터널로 연결할 수도 있습니다. 별도 터미널에서 터널을 유지하고 `.env`의 백엔드 주소를 `http://127.0.0.1:18080`으로 설정합니다.

```sh
ssh -N -L 18080:127.0.0.1:8080 user@UPPER_SERVER
```

### 4. 부팅 후 자동 연결

터미널에서 실행 중인 에이전트를 먼저 종료한 뒤, 같은 Ubuntu 사용자로 등록합니다. 서비스는 위의 고정 설치 경로와 `.env`를 사용합니다.

```sh
cd "$HOME/.local/share/marionette-agent"
mkdir -p "$HOME/.config/systemd/user"
cp marionette-agent.service "$HOME/.config/systemd/user/"
systemd-analyze --user verify "$HOME/.config/systemd/user/marionette-agent.service"
systemctl --user daemon-reload
systemctl --user enable --now marionette-agent.service
sudo loginctl enable-linger "$(id -un)"
journalctl --user -u marionette-agent.service -f
```

`enable-linger`는 로그인하지 않은 부팅 직후와 로그아웃 후에도 사용자 서비스를 유지합니다. 네트워크가 늦게 준비되어도 에이전트가 재접속합니다. 서비스의 PATH에서 찾을 수 있도록 ADB는 위의 Ubuntu 패키지(`/usr/bin/adb`)로 설치하거나 `.env`에 `MARIONETTE_ADB`의 절대 경로를 지정합니다. 재시작 제한 없이 5초 간격으로 프로세스를 다시 실행하며, 종료 시 에이전트에 먼저 SIGTERM을 보내 최대 60초 동안 정상 정리를 기다립니다. 사용자 서비스에 시스템의 `network-online.target`을 연결해 네트워크 준비를 보장한다고 가정하지 않습니다.

```sh
# 설정 변경 후 재연결
systemctl --user restart marionette-agent.service

# 상태 확인
systemctl --user status marionette-agent.service
loginctl show-user "$(id -un)" -p Linger

# 자동 실행 중지
systemctl --user disable --now marionette-agent.service
```

업그레이드할 때는 서비스가 종료된 상태에서 실행 파일과 JAR를 함께 교체하고 `.env`, 기존 상태 디렉터리, `~/.android/adbkey*`를 보존합니다. 설치 경로를 옮기면 서비스의 `WorkingDirectory`와 `ExecStart`를 함께 바꾸고 기존 상태 경로는 `MARIONETTE_STATE_DIR`로 지정합니다. 재부팅 뒤 `systemctl --user is-active marionette-agent.service`, 연결 로그, 대시보드의 같은 ID·운영 설정을 확인합니다. 실제 부팅과 USB 재연결 검증은 해당 Ubuntu 컴퓨터에서 수행해야 합니다.

## 연결 문제 확인

- `backend URL requires HTTPS except on loopback`: 상위 서버에는 HTTPS 오리진을 사용하거나 위의 SSH 터널을 사용합니다.
- `bad handshake`: 상위 백엔드 로그에서 토큰·에이전트 ID의 일치 여부, 동일 ID의 중복 접속, 프록시의 WebSocket 전달을 확인합니다.
- `x509`: 상위 서버의 인증서 체인과 도메인을 확인합니다. 사설 CA를 쓰면 Ubuntu의 신뢰 저장소에 해당 CA를 설치해야 합니다.
- 연결 로그는 있는데 기기가 없음: `adb devices -l`의 상태와 USB 케이블·인증·권한을 확인합니다.

## 개발 저장소에서 패키지 생성

```sh
# Linux amd64와 arm64를 모두 생성
make agent-package-linux

# 특정 아키텍처만 생성
GOOS=linux GOARCH=amd64 make agent-package
```

에이전트 바이너리는 `CGO_ENABLED=0`으로 빌드하며, 설치 PC의 glibc 버전에 의존하지 않습니다. Android JAR, 설정 예제, 시작·설치 스크립트, 사용자 systemd 서비스와 이 안내서를 함께 배포합니다.

패키징할 때 `build/`에 있는 Linux tar 전체에 대해 `marionette-agent-linux-SHA256SUMS`를 자동 갱신합니다. 이 파일과 목록에 있는 tar들을 같은 배포 디렉터리에 두고, 그 디렉터리에서 `sha256sum -c marionette-agent-linux-SHA256SUMS`로 검증합니다. macOS에서는 `shasum -a 256 -c marionette-agent-linux-SHA256SUMS`를 사용합니다.
