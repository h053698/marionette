macOS·Windows 사용자 PC에서 원격 Android 기기를 ADB와 Android Studio에 연결하는 Marionette CLI 첫 배포입니다.

| PC | 다운로드 파일 |
| --- | --- |
| macOS Apple Silicon (M 시리즈) | `marionette-cli-darwin-arm64.zip` |
| macOS Intel | `marionette-cli-darwin-amd64.zip` |
| Windows 64비트 (Intel / AMD) | `marionette-cli-windows-amd64.zip` |

각 ZIP에는 CLI 실행 파일, 한국어 설치 안내 `README.md`, 버전·소스 커밋 정보 `VERSION`이 들어 있습니다. Go나 소스 설치 없이 실행할 수 있습니다. ADB는 Android Studio의 SDK Manager 또는 Google 공식 Platform-Tools로 별도 설치합니다.

**[macOS / Windows 설치 안내](https://github.com/h053698/marionette/blob/cli-v0.1.0/docs/CLI_INSTALL.md)**

1. OS에 맞는 ZIP을 내려받아 압축을 풉니다.
2. ADB를 준비하고 웹 콘솔의 **API / MCP**에서 개인 토큰을 발급합니다.
3. 서버 주소와 토큰을 설정한 뒤 `marionette occupy --platform android` → `marionette adb`를 실행합니다. 웹에서 선택한 기기는 웹에서 생성한 `attach` 명령으로 넘겨받습니다.
4. 연결 명령을 켜 둔 상태로 사용하고, 사용을 마치면 `marionette release`로 반납합니다.

Windows의 기본 Android SDK 경로·`adb.exe` 자동 탐색과 세션 ACL 처리를 포함합니다. macOS에서는 기존 0600 세션 파일 권한 검사를 유지합니다. `marionette --version`으로 버전과 소스 커밋을 확인할 수 있습니다.

`SHA256SUMS`는 ZIP의 SHA-256 체크섬입니다. 실행 파일은 코드 서명·공증되지 않았습니다. Windows의 실제 원격 기기 연결과 Android Studio 자체의 실행·디버깅 UI는 이번 릴리즈의 실기기 검증 범위에 포함하지 않습니다. `adb reverse`는 지원하지 않습니다.
