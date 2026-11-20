# PC용 Marionette 에이전트 설치

이 프로그램은 Android 휴대폰이 USB로 연결되는 **macOS 또는 Linux 컴퓨터**에서 실행합니다. 프론트엔드와 중계 백엔드는 별도로 실행합니다.

Ubuntu에서는 관리자가 **에이전트 운영 → 에이전트 연결**에서 PC를 등록한 뒤 **Ubuntu (권장)** 탭의 명령을 복사해 대상 PC의 일반 사용자 터미널에서 실행합니다. `curl`이 필요하며, 설치 중 `sudo`가 필요한 작업은 실행 전에 확인을 요청합니다. Debian에서도 같은 스크립트를 쓸 수 있습니다. 자세한 내용은 [Ubuntu 설치·연결 안내](UBUNTU_AGENT.md#한-번에-설치)를 참고하세요.

## 패키지로 직접 설치

연결 창에서 **직접 설치(패키지)**를 선택하고 아래 절차를 따릅니다. Ubuntu와 Debian에서는 푼 Linux 패키지에서 `bash setup-ubuntu-agent.sh`로 설치하고 사용자 systemd 서비스를 등록할 수 있습니다.

1. Android SDK Platform Tools의 `adb`를 설치하고 PATH에 추가합니다. `adb devices -l`로 휴대폰이 표시되는지 확인합니다. 휴대폰에서 USB 디버깅과 컴퓨터 인증을 허용해야 합니다.
2. 컴퓨터의 OS와 CPU에 맞는 `marionette-agent-<os>-<arch>.tar.gz`를 풀고 고정된 설치 경로에 둡니다.
3. 관리자가 웹 콘솔의 **에이전트 운영 → 에이전트 연결**에서 이 PC의 ID와 이름을 등록합니다. 한 번 표시되는 토큰과 연결 명령을 보관합니다. 패키지 안의 `.env.example`을 `.env`로 복사하고 백엔드 주소, 등록한 ID, 해당 컴퓨터의 토큰을 입력합니다. `.env`는 `chmod 600 .env`로 보호합니다.
4. `bash start-agent.sh`로 실행합니다. 별도 ADB 경로는 `bash start-agent.sh --adb /absolute/path/to/adb`로 지정합니다.
5. 웹 콘솔의 **에이전트 운영**에서 PC의 연결 상태를 확인하고 **운영 설정**을 엽니다. 실물 전용·에뮬레이터 전용·혼합 중에서 선택합니다.

패키지에는 Go 실행 파일과 Android용 DEX JAR가 들어 있습니다. 실물 기기를 연결할 때는 설치 컴퓨터에 Go, JDK, Android 빌드 SDK가 필요하지 않습니다. ADB와 휴대폰 연결에 필요한 OS 드라이버·USB 권한은 설치되어 있어야 합니다. 에뮬레이터를 생성할 때는 아래의 추가 도구가 필요합니다.

에이전트는 백엔드에 WebSocket 연결을 먼저 엽니다. PC에 HTTP 수신 포트를 열지 않습니다. 외부 백엔드 주소에는 HTTPS가 필요하며 로컬 개발에서는 `http://127.0.0.1:8080`을 사용할 수 있습니다. 연결이 끊어지면 기존 점유와 기기 런타임을 종료하고 1–30초 간격으로 재접속합니다. 사용자는 다시 점유해야 합니다.

ADB 경로, ICE 서버, 상태 디렉터리, 전송 한도는 [에이전트 환경 변수](CONFIG.md#에이전트-marionette-agent)에서 확인할 수 있습니다.

상시 실행은 아래 macOS launchd 절차 또는 [Ubuntu 사용자 systemd 절차](UBUNTU_AGENT.md#4-부팅-후-자동-연결)를 사용합니다. 실행 사용자에게 ADB와 USB 접근 권한이 있어야 합니다. 종료는 SIGINT/SIGTERM으로 처리하며 자신이 시작한 드라이버와 포트 포워딩을 정리합니다. 서비스는 정상 정리에 최대 60초를 허용한 뒤 남은 프로세스를 종료합니다.

개발 저장소에서 패키지를 만들려면 `make tools` 후 `make agent-package`를 실행합니다. 다른 대상은 `GOOS=linux GOARCH=amd64 make agent-package`처럼 지정할 수 있습니다. 결과는 `build/marionette-agent-<os>-<arch>.tar.gz`입니다.

`make agent-package-linux`는 Ubuntu용 Linux amd64와 arm64 패키지 및 체크섬 목록을 `build/`에 생성합니다. Ubuntu 설치 명령이 외부에서 작동하려면 HTTPS 역방향 프록시에서 `/install/ubuntu.sh`와 `/downloads/*`를 백엔드로 전달하세요. 패키지 위치는 백엔드의 `--agent-packages`로 지정할 수 있습니다.

## macOS 로그인 후 자동 연결

패키지를 `$HOME/.local/share/marionette-agent` 등 고정 경로에 풀고 위의 수동 연결을 먼저 확인합니다. 터미널 에이전트를 종료한 후 **ADB 인증에 사용한 동일한 일반 사용자**로 등록합니다. LaunchAgent는 로그인 시 시작하고 로그아웃하면 종료합니다. FileVault 잠금 해제 전이나 사용자 로그인 전의 부팅 실행은 이 구성의 대상이 아닙니다. 잠자기 중에는 기기를 제공할 수 없습니다.

서비스에는 터미널의 셸 설정이 전달되지 않습니다. 패키지의 `.env`에 필요한 경로를 명시합니다. 이미 운영 중이면 기존 `--state-dir`의 절대 경로를 그대로 사용합니다. 경로에 공백이 있으면 아래처럼 따옴표로 감쌉니다. `.env`는 Bash로 읽으므로 신뢰하는 설정만 넣습니다.

```sh
# 실물 기기만 쓰면 실제 설치된 adb 경로만 지정하면 됩니다.
MARIONETTE_ADB="$HOME/Library/Android/sdk/platform-tools/adb"
# 에뮬레이터를 관리할 때 추가합니다. Android Studio의 SDK Location과 맞춥니다.
MARIONETTE_ANDROID_SDK="$HOME/Library/Android/sdk"
JAVA_HOME="/Applications/Android Studio.app/Contents/jbr/Contents/Home"
# 기존 상태 경로가 있으면 그 경로를 사용합니다.
MARIONETTE_STATE_DIR="$HOME/.local/share/marionette-agent/.marionette/agent-office-mac"
MARIONETTE_MAX_EMULATORS=2
```

`MARIONETTE_ADB`, `MARIONETTE_ANDROID_SDK`, `MARIONETTE_STATE_DIR`, `MARIONETTE_MAX_EMULATORS`는 시작 스크립트가 실제 바이너리 플래그로 전달합니다. 수동으로 추가한 플래그가 있으면 플래그가 우선합니다. 상태 경로를 생략하면 패키지 설치 디렉터리의 `.marionette/agent-<id>`를 사용하며 시작한 터미널 위치에 따라 바뀌지 않습니다.

아래 생성 단계에는 Python 3가 필요합니다. 생성기는 절대 경로와 로그 경로가 들어간 plist를 만들며 기존 파일을 덮어쓰거나 서비스를 등록하지 않습니다. 로그는 설치 경로의 `.marionette/logs/agent.log`에 쌓입니다. 장기 운영 시 별도 로그 순환 정책을 적용합니다.

```sh
cd "$HOME/.local/share/marionette-agent"
python3 render-launchd.py agent --install-dir "$PWD" --output ./io.marionette.agent.plist
plutil -lint ./io.marionette.agent.plist
plutil -p ./io.marionette.agent.plist

# 생성 결과와 .env를 확인한 후 현재 사용자의 로그인 서비스로 등록합니다.
mkdir -p "$HOME/Library/LaunchAgents"
cp -n ./io.marionette.agent.plist "$HOME/Library/LaunchAgents/"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/io.marionette.agent.plist"
launchctl print "gui/$(id -u)/io.marionette.agent"
tail -f .marionette/logs/agent.log
```

`cp -n`으로 기존 plist가 유지되었다면 그 파일의 경로도 확인합니다. 기존 설정을 교체할 때는 먼저 `launchctl bootout "gui/$(id -u)/io.marionette.agent"`로 해당 서비스를 내린 뒤 검토한 파일을 복사하고 다시 `bootstrap`합니다. `.env`만 바꿀 때도 정상 정리를 위해 같은 `bootout` → `bootstrap` 순서를 사용합니다. 자동 시작을 제거하려면 `bootout` 후 `~/Library/LaunchAgents/io.marionette.agent.plist`를 제거합니다.

소스 저장소에서 직접 운영할 때는 `make agent device`로 먼저 빌드하고 `.env.agent.example`을 `.env.agent`로 복사해 설정합니다. 실행은 `bash scripts/start-agent.sh`, 생성기는 `python3 scripts/render-launchd.py agent --install-dir "$PWD" --output ./io.marionette.agent.plist`입니다. 백엔드·프론트엔드·러너 자동 시작은 소스 저장소의 `deploy/README.md`를 따릅니다.

## 대시보드에서 PC 운영 방식 정하기

관리자에게만 에이전트 운영 메뉴가 표시됩니다. 등록된 PC의 연결 여부, 호스트명, OS·CPU, 준비된 컨트롤러 수, 영상 시청자 수와 관리 중인 에뮬레이터를 확인할 수 있습니다. 기기를 점유한 사용자가 있으면 먼저 모두 반납해야 운영 설정을 변경할 수 있습니다.

| 설정 | 동작 |
| --- | --- |
| 실물 전용 | USB 휴대폰만 기기 목록과 배정 대상으로 표시합니다. 에이전트가 만든 에뮬레이터는 종료합니다. |
| 에뮬레이터 전용 | 가상 기기만 표시합니다. 선택한 이미지와 수량만큼 관리용 에뮬레이터를 생성하고 실행 상태를 유지합니다. |
| 혼합 | USB 휴대폰과 에뮬레이터를 함께 표시합니다. 유지할 관리용 가상 기기 수를 별도로 정합니다. |
| 실물 기기는 충전 중 화면 켜두기 | 연결된 실물 기기의 절전을 막습니다. 처음 설정하기 전 값을 기록하고 옵션을 끄거나 에이전트를 정상 종료할 때 복원합니다. PIN·비밀번호 잠금은 해제하지 않습니다. |
| 컨트롤러 상시 대기 | 기기별 프로세스, UiAutomation 연결, H.264 인코더를 미리 준비합니다. 사용자가 반납해도 유지하며 점유 권한은 별도로 검사합니다. 기본으로 켜져 있습니다. |

에이전트가 만든 가상 기기는 자신의 전용 디렉터리에서 관리합니다. 이미 실행 중인 다른 AVD를 종료하거나 삭제하지 않습니다. 관리용 에뮬레이터가 종료되면 설정한 수량에 맞춰 다시 실행합니다. 백엔드 연결이 잠깐 끊겨도 에뮬레이터 감독은 계속되며, 에이전트 프로세스를 정상 종료하면 자신이 실행한 에뮬레이터도 종료합니다. PC 재부팅 뒤에도 운영하려면 위의 systemd·launchd 자동 시작을 설정합니다.

기본 기기 목록에는 ADB가 감지한 실물 기기와 이 에이전트가 실행한 에뮬레이터가 들어갑니다. 전용 팜 PC에 개발용 기기도 연결되어 있다면 `MARIONETTE_DEVICE_ALLOW`로 범위를 제한하세요. 쉼표로 구분해 기기 시리얼, `physical:*`(모든 실물 기기), `managed-only`(이 에이전트가 실행한 에뮬레이터)를 지정할 수 있습니다. 명시한 목록은 기본 목록을 대체하며 위의 운영 모드도 함께 적용됩니다. 예를 들어 `MARIONETTE_DEVICE_ALLOW=physical:*,managed-only,emulator-5580`은 기본 범위에 별도로 실행한 `emulator-5580`을 추가합니다. 상시 대기는 허용된 실물과 관리 에뮬레이터에 적용됩니다. 충전 중 화면 켜두기는 허용된 실물에 적용되며 절전 설정 기록(`awake.json`)이 잘못되면 기록을 보존하고 로그에 경고를 남긴 뒤 이 기능을 비활성화합니다.

## 에뮬레이터용 SDK 준비

이 PC에 Android SDK의 **Platform Tools, Emulator, Command-line Tools, 호환되는 시스템 이미지와 JDK**를 설치합니다. SDK Manager로 설치된 패키지를 사용하세요. 단순히 실행 파일만 복사하면 `avdmanager`가 설치된 패키지를 인식하지 못할 수 있습니다. Apple Silicon에는 `arm64-v8a`, 일반 Intel·AMD Linux에는 `x86_64` 이미지가 필요합니다. 대시보드에서는 설치된 이미지 중에서 선택하며 이미지를 자동으로 내려받지 않습니다.

```sh
bash start-agent.sh \
  --adb /absolute/android-sdk/platform-tools/adb \
  --android-sdk /absolute/android-sdk \
  --state-dir /absolute/private/agent-state \
  --max-emulators 2
```

`JAVA_HOME`은 설치한 JDK로 지정합니다. 가상 기기는 기본적으로 대당 메모리 1.5GB, CPU 2코어, 720×1280 화면을 사용합니다. `--max-emulators 0`이면 이 PC에서 생성 기능을 끌 수 있고 최대값은 32입니다. 대당 메모리와 CPU를 쓰므로 PC 사양에 맞게 정하세요. 설정과 AVD 데이터는 `--state-dir`에 남으므로 같은 디렉터리를 계속 사용합니다. 기본값은 실행 디렉터리 기준 `.marionette/agent-<id>`입니다. 같은 상태 디렉터리를 두 에이전트가 동시에 사용할 수 없습니다.

Android Studio를 설치했다면 **Settings → Languages & Frameworks → Android SDK**의 SDK Location과 위 경로를 맞춥니다. SDK Tools에서 Platform-Tools, Android Emulator, Android SDK Command-line Tools를 확인하고 SDK Platforms에서 호스트 CPU에 맞는 시스템 이미지를 설치합니다. Studio의 JBR를 쓰는 경우에도 실제 설치 경로가 존재하는지 확인합니다. Studio 자체는 USB 에이전트 실행에 필요하지 않습니다. 설치 조건은 [공식 설치 안내](https://developer.android.com/studio/install), 가속 조건은 [공식 Emulator 가속 안내](https://developer.android.com/studio/run/emulator-acceleration)를 참고합니다.

```sh
"$MARIONETTE_ADB" version
"$MARIONETTE_ANDROID_SDK/emulator/emulator" -accel-check
"$JAVA_HOME/bin/java" -version
```

위 명령은 `.env`를 읽은 셸에서 실행합니다(`set -a; . ./.env; set +a`). 도구 존재와 가속 점검은 Android Studio GUI에서 원격 기기를 배정받아 앱을 실행·디버깅한 결과와 별개입니다. 원격 ADB 연결 후 Studio의 실행 대상에 배정된 기기가 표시되는지는 [자동화·원격 ADB 안내](AUTOMATION.md)에 따라 확인합니다.

## 업그레이드와 재부팅 확인

설치 경로와 실행 사용자를 유지하고, 에이전트를 정상 종료한 상태에서 실행 파일·JAR·시작 스크립트를 함께 교체합니다. `.env`, `.marionette/agent-<id>` 또는 지정한 상태 디렉터리, 사용자의 `~/.android/adbkey*`는 보존합니다. 새 키 생성이나 ADB 서버 초기화는 업그레이드 절차에 포함하지 않습니다. 상태 디렉터리에 기존 기기의 절전 설정 복원 기록도 있으므로 삭제하지 않습니다.

재시작 후 로그의 `agent connected to relay`, 대시보드의 같은 ID, 기기 목록과 운영 설정·AVD 데이터 유지를 확인합니다. 실제 재부팅 검증은 작업 중인 점유가 없는 유지보수 시간에 별도로 수행합니다. macOS는 로그인 후, Ubuntu는 linger를 활성화했다면 로그인 전부터 사용자 서비스가 시작됩니다. 실물 기기의 최초 USB 인증과 PIN·비밀번호는 사용자가 직접 처리합니다.

## 연결과 기기 조작의 범위

관리자가 등록한 에이전트는 백엔드의 `--agent-file`(기본 `.marionette/agents.json`)에 토큰 해시만 저장되어 재시작 후에도 유지됩니다. 기존 `MARIONETTE_AGENT_TOKENS` 등록 방식도 사용할 수 있습니다. 새 PC를 웹에서 등록할 때 백엔드를 재시작할 필요는 없습니다. 한 ID는 한 PC에서만 동시에 사용할 수 있습니다.

실물 Android 기기는 ADB로 드라이버를 시작하고, 드라이버 안에서 UiAutomation과 InputManager를 사용합니다. Android 13 이하의 지원 기기는 SurfaceControl, 이후 버전은 DisplayManager의 VirtualDisplay로 화면을 MediaCodec H.264 인코더에 연결합니다. 한 기기의 영상 스트림을 여러 브라우저에 배포하며 느린 시청자의 버퍼는 다른 시청자를 막지 않습니다.

기기를 점유하면 상세 창에서 앱 이름·패키지로 검색하고 실행할 수 있습니다. 마우스를 올리면 웹 버튼 설명이 표시되고, 기기 화면 위의 마우스 이동은 Android의 실제 hover 이벤트로 전달됩니다. 앱 자체가 hover를 지원해야 해당 앱의 설명이 나타납니다. 앱 실행과 hover도 점유한 사용자만 사용할 수 있습니다.

Android Studio에서 배정된 폰을 쓰는 방법과 사용 가능한 ADB 범위는 [자동화·원격 ADB 안내](AUTOMATION.md)를 참고하세요. root·remount·재부팅 같은 명령은 실제 기기 권한과 상태를 따르며, recovery·bootloader·fastboot는 일반 adbd 중계와 다른 연결입니다.

## 운영 보안 주의

- 실물과 에뮬레이터의 네트워크는 별도 VLAN이나 테스트 전용 Wi-Fi로 사내 업무망에서 격리하세요. 에뮬레이터의 호스트 경유 트래픽에도 같은 접근 제한을 적용하세요.
- 점유가 끝나면 조작 연결과 진행 중인 입력은 정리되지만 기기 초기화는 실행하지 않습니다. 설치 앱, 계정, 파일과 앱 데이터는 다음 사용자에게 남을 수 있으므로 서로 신뢰하지 않는 사용자에게 재배정하기 전에 별도로 초기화하세요.
- 파일 API의 `/sdcard` 제한은 경로 문자열 검사입니다. 심볼릭 링크와 마운트가 있는 기기에서 파일 접근을 격리하는 경계로 사용하지 마세요. `overwrite=0`인 업로드는 에이전트의 동시 저장을 직렬화하며, ADB나 앱이 같은 파일을 동시에 바꾸는 환경에서는 Android 파일시스템과 `mv` 구현의 동작도 영향을 줍니다.
