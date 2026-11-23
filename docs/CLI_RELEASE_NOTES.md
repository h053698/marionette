macOS 터미널과 Windows PowerShell에서 CLI를 설치하고 업데이트할 수 있습니다. 최신 버전의 ZIP을 내려받아 SHA-256을 확인하고, 관리자 권한 없이 사용자 폴더에 설치한 뒤 PATH에 등록합니다.

macOS:

```sh
curl -fsSL https://github.com/h053698/marionette/releases/latest/download/install.sh | sh
export PATH="$HOME/.local/bin:$PATH"
marionette --version
```

Windows PowerShell:

```powershell
irm https://github.com/h053698/marionette/releases/latest/download/install.ps1 | iex
marionette --version
```

같은 명령을 다시 실행하면 업데이트됩니다. macOS는 `~/.local/bin`과 zsh/Bash 설정, Windows는 `%LOCALAPPDATA%\Marionette\bin`과 사용자·현재 프로세스 PATH를 사용합니다. 개인 토큰과 기기 세션은 변경하지 않습니다.

macOS Apple Silicon·Intel, Windows Intel/AMD 64비트 ZIP도 함께 제공합니다. 각 ZIP에는 실행 파일, 한국어 설치 안내, 버전·소스 커밋이 들어 있습니다. ADB는 Android Studio의 SDK Manager 또는 [Google 공식 Platform-Tools](https://developer.android.com/tools/releases/platform-tools)로 별도 설치합니다.

**[전체 설치·기기 연결 안내](https://github.com/h053698/marionette/blob/main/docs/CLI_INSTALL.md)**

맥과 Windows PowerShell 5.1에서 설치·재설치·PATH 등록·체크섬 불일치 시 기존 설치 보존을 자동 검증합니다. Windows의 실제 원격 기기 연결과 Android Studio 실행·디버깅 UI는 별도 환경 검증이 필요합니다. 실행 파일은 코드 서명·공증되지 않았습니다. `adb reverse`는 지원하지 않습니다.
