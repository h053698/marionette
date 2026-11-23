# Marionette CLI 설치 — macOS / Windows

사용자 PC에서 원격 Android 기기를 ADB와 Android Studio에 연결하는 CLI입니다. Go, Java 또는 Marionette 소스를 설치할 필요가 없습니다.

## 1. CLI 다운로드

[GitHub Releases](https://github.com/h053698/marionette/releases/latest)에서 PC에 맞는 ZIP을 받아 압축을 풉니다.

| PC | 다운로드 |
| --- | --- |
| macOS Apple Silicon (M 시리즈) | [marionette-cli-darwin-arm64.zip](https://github.com/h053698/marionette/releases/latest/download/marionette-cli-darwin-arm64.zip) |
| macOS Intel | [marionette-cli-darwin-amd64.zip](https://github.com/h053698/marionette/releases/latest/download/marionette-cli-darwin-amd64.zip) |
| Windows 64비트 (Intel / AMD) | [marionette-cli-windows-amd64.zip](https://github.com/h053698/marionette/releases/latest/download/marionette-cli-windows-amd64.zip) |

압축을 푼 폴더에서 터미널 또는 PowerShell을 엽니다. 이 안내는 해당 폴더에서 실행하는 명령을 사용하므로 PATH 등록은 필요하지 않습니다.

```sh
# macOS
chmod +x ./marionette
./marionette --version
```

```powershell
# Windows PowerShell
.\marionette.exe --version
```

실행 파일은 현재 코드 서명·공증되지 않았으므로 macOS 또는 Windows에서 실행 확인이 표시될 수 있습니다. 게시자와 다운로드 출처를 확인하고 운영체제의 실행 허용 절차를 따릅니다.

릴리즈의 `SHA256SUMS`로 ZIP의 SHA-256을 비교할 수 있습니다. macOS는 `shasum -a 256 다운로드한파일.zip`, PowerShell은 `Get-FileHash .\다운로드한파일.zip -Algorithm SHA256`을 사용합니다.

## 2. ADB 준비

Android Studio 사용자는 **SDK Manager → SDK Tools → Android SDK Platform-Tools**를 설치합니다. 기본 SDK 위치와 `ANDROID_HOME`, `ANDROID_SDK_ROOT`, PATH에 있는 ADB는 CLI가 자동으로 찾습니다.

Android Studio가 없다면 [Google 공식 Platform-Tools 다운로드](https://developer.android.com/tools/releases/platform-tools)에서 OS에 맞는 ZIP을 받아 압축을 풉니다. 이 경우 아래 연결 명령에 `--adb`로 `adb` 또는 `adb.exe`의 경로를 지정해도 됩니다.

CLI ZIP에는 Google의 Platform-Tools가 포함되어 있지 않습니다.

## 3. 서버 주소와 개인 토큰 설정

Marionette 웹 콘솔에 로그인하고 **API / MCP**에서 개인 토큰을 발급합니다. 아래 서버 주소는 관리자가 안내한 **백엔드 주소**로 바꿉니다. 토큰은 CLI를 사용하는 계정에서 발급해야 합니다.

macOS 터미널:

```sh
export MARIONETTE_URL="https://marionette.example.com"
read -rs 'MARIONETTE_TOKEN?개인 API 토큰: '; echo
export MARIONETTE_TOKEN
```

위 입력 명령은 macOS 기본 셸인 zsh 기준입니다. Bash에서는 `read -rs -p '개인 API 토큰: ' MARIONETTE_TOKEN; echo`를 사용합니다.

Windows PowerShell:

```powershell
$env:MARIONETTE_URL = "https://marionette.example.com"
$env:MARIONETTE_TOKEN = [System.Net.NetworkCredential]::new("", (Read-Host "개인 API 토큰" -AsSecureString)).Password
```

이 설정은 현재 터미널에만 적용됩니다. 새 터미널에서는 다시 설정하세요. 개인 토큰은 실행 파일이나 공유 설정 파일에 넣지 않습니다.

## 4. 기기 연결

사용 가능한 기기 하나를 배정받아 연결합니다.

macOS:

```sh
./marionette occupy --platform android
./marionette adb
```

Windows:

```powershell
.\marionette.exe occupy --platform android
.\marionette.exe adb
```

ADB가 자동으로 발견되지 않으면 경로를 지정합니다. Android Studio의 SDK 위치를 바꿨다면 실제 경로를 사용하세요.

```sh
# macOS Android Studio 기본 SDK 위치
./marionette adb --adb "$HOME/Library/Android/sdk/platform-tools/adb"
```

```powershell
# Windows Android Studio 기본 SDK 위치
.\marionette.exe adb --adb "$env:LOCALAPPDATA\Android\Sdk\platform-tools\adb.exe"
```

명령이 출력한 `127.0.0.1:<포트>`가 로컬 ADB에 연결됩니다. Android Studio에서는 연결된 기기를 선택합니다. ADB 명령을 직접 쓸 때는 같은 ADB 실행 파일과 출력된 주소를 사용합니다. 예: `adb -s 127.0.0.1:15555 shell`.

**`adb` 연결 명령은 사용하는 동안 켜 두세요.** 실행 중에는 기기 점유를 갱신합니다. Ctrl+C로 연결을 끊고, 아래 명령으로 즉시 기기를 반납합니다.

```sh
# macOS
./marionette release
```

```powershell
# Windows
.\marionette.exe release
```

### 웹에서 선택한 기기를 연결하는 경우

CLI와 ADB 설치 및 토큰 설정을 먼저 마칩니다. 웹에서 **점유하고 조작 → Android Studio · ADB → 연결 코드 만들기**를 누릅니다. 표시된 명령의 `--device`와 `--code` 값을 아래에 넣어 실행합니다.

```sh
# macOS
./marionette attach --device "기기-ID" --code "연결-코드"
./marionette adb
```

```powershell
# Windows PowerShell
.\marionette.exe attach --device "기기-ID" --code "연결-코드"
.\marionette.exe adb
```

웹에서 복사하는 전체 명령은 macOS/Linux 셸 문법입니다. Windows에서는 앞 단계의 PowerShell 환경 변수 설정과 위 명령을 사용합니다.

연결 코드는 90초 동안 한 번만 유효합니다. 만료되면 웹에서 새 코드를 만드세요. 웹과 CLI의 개인 토큰은 같은 계정이어야 하며, 넘겨받으면 브라우저의 조작 연결은 종료됩니다.

## 세션과 연결 참고

- 점유 정보는 현재 폴더의 `.marionette/session.json`에 저장됩니다. 배정·연결·반납 명령은 같은 폴더에서 실행하세요. 다른 폴더에서 사용할 때는 모든 명령에 같은 `--session 절대경로`를 명령 이름 앞에 지정합니다.
- macOS는 세션 파일을 0600으로 저장하고, Windows는 현재 사용자와 SYSTEM만 접근하도록 ACL을 설정합니다. 개인 API 토큰은 이 파일에 저장하지 않습니다.
- 기본 연결은 WebRTC 직접 연결을 시도하고 실패하면 서버 중계로 전환합니다. 회사망에서 직접 연결이 어려우면 `adb --mode relay`를 사용합니다.
- `adb reverse`는 지원하지 않습니다.
- Android Studio 자체의 실행·디버깅 UI와 Windows에서의 실기기 ADB 연결은 별도 환경 검증이 필요합니다. 기존 실기기 확인 범위는 셸·화면 캡처·파일 전송·반납 후 연결 종료입니다.

추가 명령과 동작은 [자동화 안내](https://github.com/h053698/marionette/blob/main/docs/AUTOMATION.md)를 참고하세요.
