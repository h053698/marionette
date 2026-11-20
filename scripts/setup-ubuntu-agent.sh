#!/usr/bin/env bash
# Disable tracing before handling credentials, even if invoked with bash -x.
set +x
set -euo pipefail
umask 077
source_script=${BASH_SOURCE[0]:-}
stage=''

die() { printf '오류: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

usage() {
  cat <<'EOF'
사용법: bash setup-ubuntu-agent.sh [옵션]
  --backend URL             백엔드 오리진 (MARIONETTE_BACKEND_URL)
  --id ID                   등록한 에이전트 ID (MARIONETTE_AGENT_ID)
  --token TOKEN             에이전트 토큰 (MARIONETTE_AGENT_TOKEN 또는 숨김 입력 권장)
  --mode physical|emulator  설치할 도구 (기본 physical)
  --emulators N             관리용 에뮬레이터 상한 0~8 (MARIONETTE_MAX_EMULATORS)
  --tags TAGS               쉼표로 구분한 기기 태그 (MARIONETTE_DEVICE_TAGS)
  --yes                     확인 생략, sudo는 -n 사용
  --dry-run                 변경 없이 작업 계획 출력
  --uninstall [--purge]     서비스와 설치 파일 제거 (--purge는 SDK/JDK도 삭제)
  --help                    도움말
EOF
}

confirm() {
  local answer
  (( assume_yes )) && return 0
  printf '%s [y/N] ' "$1" >&2
  IFS= read -r answer </dev/tty || die '대화형 터미널이나 --yes 옵션이 필요합니다.'
  [[ $answer == [yY] || $answer == [yY][eE][sS] ]]
}

show_command() {
  printf '  %q' "$@"
  printf '\n'
}

check_install_dir() {
  [[ ! -L $install_dir ]] || die '설치 경로의 심볼릭 링크를 해제한 뒤 다시 실행하세요.'
  [[ ! -e $install_dir || -d $install_dir ]] || die "디렉터리가 필요합니다: $install_dir"
}

check_user_systemd() {
  systemctl --user show-environment >/dev/null 2>&1 ||
    die 'systemd 사용자 세션에 연결할 수 없습니다. 설치할 일반 사용자로 로그인하세요.'
}

lock_installation() {
  mkdir -p -- "$install_parent"
  # Keep an open descriptor in the installer process for the entire operation.
  exec {install_lock}<"$install_parent"
  flock --nonblock "$install_lock" || die '다른 Marionette 설치 또는 제거가 진행 중입니다.'
}

uninstall_agent() {
  say "사용자 서비스 중지 및 자동 시작 해제: $unit"
  say "설치 파일, .env 및 설치 폴더 안의 상태 삭제: $install_dir"
  if (( purge )); then
    say '설치 폴더의 Android SDK와 JDK도 삭제합니다.'
  else
    say '설치 폴더의 android-sdk와 jdk는 보존합니다.'
  fi
  (( dry_run )) && return 0
  confirm '제거를 진행할까요?' || die '제거를 취소했습니다.'
  check_install_dir
  check_user_systemd
  if [[ -d $install_parent ]]; then lock_installation; fi
  if [[ -e $unit_file || -L $unit_file ]] || systemctl --user is-active --quiet "$unit" ||
    systemctl --user is-enabled --quiet "$unit"; then
    systemctl --user disable --now "$unit"
  fi
  rm -f -- "$unit_file"
  systemctl --user daemon-reload
  if (( purge )); then
    rm -rf -- "$install_dir"
  elif [[ -d $install_dir ]]; then
    find "$install_dir" -mindepth 1 -maxdepth 1 ! -name android-sdk ! -name jdk -exec rm -rf -- {} +
    rmdir --ignore-fail-on-non-empty -- "$install_dir"
  fi
  say '제거했습니다. 시스템 패키지, 그룹, linger 및 설치 폴더 밖의 상태는 유지됩니다.'
}

read_existing_config() (
  cd "$install_dir"
  unset "${config_keys[@]}"
  {
    # start-agent.sh also reads this trusted, user-owned file as Bash.
    # shellcheck source=/dev/null
    source "$env_file" || exit 1
    set +x
  } </dev/null >/dev/null 2>&1
  for key in "${config_keys[@]}"; do
    if [ -v "$key" ]; then printf '%s=%s\0' "$key" "${!key}"; fi
  done
  printf '%s\0' complete
)

load_config() {
  local key entry
  local -a saved=()
  if [[ -e $env_file || -L $env_file ]]; then
    [[ -f $env_file && ! -L $env_file && -O $env_file ]] ||
      die ".env는 현재 사용자 소유의 일반 파일이어야 합니다: $env_file"
    if (( ! dry_run )); then
      mapfile -d '' -t saved < <(read_existing_config)
      [[ ${saved[-1]:-} == complete ]] || die '.env 설정을 읽지 못했습니다.'
      for entry in "${saved[@]}"; do
        [[ $entry == *=* ]] || continue
        previous[${entry%%=*}]=${entry#*=}
      done
    fi
  fi
  for key in "${config_keys[@]}"; do
    if [[ ${previous[$key]+set} ]]; then config[$key]=${previous[$key]}; fi
    if [[ ${inputs[$key]+set} ]]; then config[$key]=${inputs[$key]}; fi
  done
}

require_config() {
  local key=$1 prompt=$2 answer
  [[ -n ${config[$key]:-} ]] && return 0
  (( dry_run )) && return 0
  (( ! assume_yes )) || die "$key 설정이 필요합니다."
  printf '%s: ' "$prompt" >&2
  if [[ $key == MARIONETTE_AGENT_TOKEN ]]; then
    IFS= read -rs answer </dev/tty || die '토큰을 입력하거나 MARIONETTE_AGENT_TOKEN을 설정하세요.'
    printf '\n' >&2
  else
    IFS= read -r answer </dev/tty || die "$key 설정이 필요합니다."
  fi
  [[ -n $answer ]] || die "$key 설정이 필요합니다."
  config[$key]=$answer
}

validate_config() {
  local LC_ALL=C
  local backend=${config[MARIONETTE_BACKEND_URL]:-} id=${config[MARIONETTE_AGENT_ID]:-}
  local token=${config[MARIONETTE_AGENT_TOKEN]:-} max=${config[MARIONETTE_MAX_EMULATORS]:-}
  local https='^https://([a-zA-Z0-9._-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]+)?/?$'
  local local_http='^http://(localhost|127(\.[0-9]{1,3}){3}|\[::1\])(:[0-9]+)?/?$'
  if [[ -n $backend && ! $backend =~ $https && ! ${backend,,} =~ $local_http ]]; then
    die '백엔드 주소는 HTTPS 오리진이어야 합니다. 루프백만 HTTP를 사용할 수 있습니다.'
  fi
  [[ -z $id || $id =~ $id_pattern ]] || die '에이전트 ID 형식이 올바르지 않습니다.'
  if [[ -n $token && ( ${#token} -lt 32 || $token == *[[:space:]]* ) ]]; then
    die '토큰은 공백 없이 32바이트 이상이어야 합니다.'
  fi
  [[ -z $max || $max =~ ^[0-8]$ ]] || die '--emulators 값은 0~8이어야 합니다.'
}

configure_paths() {
  local key path resolved old_id=${previous[MARIONETTE_AGENT_ID]:-}
  if [[ $mode == emulator ]]; then
    config[MARIONETTE_ANDROID_SDK]=$sdk_dir
    config[JAVA_HOME]=$jdk_dir
    config[MARIONETTE_ADB]=$sdk_dir/platform-tools/adb
  elif [[ $mode_option == physical || $fallback == 1 ]]; then
    config[MARIONETTE_ADB]=${inputs[MARIONETTE_ADB]:-/usr/bin/adb}
  elif [[ -z ${config[MARIONETTE_ADB]:-} ]]; then
    if [[ -n ${config[MARIONETTE_ANDROID_SDK]:-} ]]; then
      config[MARIONETTE_ADB]=${config[MARIONETTE_ANDROID_SDK]}/platform-tools/adb
    else
      config[MARIONETTE_ADB]=/usr/bin/adb
    fi
  fi
  if [[ ! ${config[MARIONETTE_STATE_DIR]:-} ]]; then
    if [[ ! $old_id =~ $id_pattern ]]; then old_id=${config[MARIONETTE_AGENT_ID]:-'<ID>'}; fi
    config[MARIONETTE_STATE_DIR]=$install_dir/.marionette/agent-$old_id
  fi
  for key in MARIONETTE_ADB MARIONETTE_ANDROID_SDK JAVA_HOME MARIONETTE_STATE_DIR; do
    path=${config[$key]:-}
    [[ -n $path && $path != /* ]] || continue
    if [[ $key == MARIONETTE_ADB && $path != */* ]]; then
      resolved=$(type -P -- "$path") || die '기존 ADB 설정을 확인할 수 없습니다. MARIONETTE_ADB에 절대 경로를 지정하세요.'
      [[ $resolved == /* ]] || die 'MARIONETTE_ADB에 절대 경로를 지정하세요.'
      config[$key]=$resolved
    else
      config[$key]=$install_dir/$path
    fi
  done
}

check_saved_emulator_count() {
  local policy=${config[MARIONETTE_STATE_DIR]}/policy.json
  local count_pattern='"emulatorCount"[[:space:]]*:[[:space:]]*([0-8])([^0-9]|$)'
  if [[ -f $policy && $(<"$policy") =~ $count_pattern ]] &&
    (( BASH_REMATCH[1] > config[MARIONETTE_MAX_EMULATORS] )); then
    die '대시보드의 에뮬레이터 실행 수를 새 상한 이하로 낮춘 뒤 다시 실행하세요.'
  fi
}

stage_config() {
  local key line changed_count=0
  local canonical='^(export[[:blank:]]+)?([A-Z_][A-Z0-9_]*)=[a-zA-Z0-9_./,:@%+=~-]*$'
  local managed='^([A-Z_][A-Z0-9_]*)=.* # marionette-ubuntu-installer$'
  local -A changed=()
  for key in "${config_keys[@]}"; do
    if [[ ${config[$key]+set} && ( ! ${previous[$key]+set} || ${config[$key]} != "${previous[$key]:-}" ) ]]; then
      changed[$key]=1
      changed_count=$((changed_count + 1))
    fi
  done
  if (( changed_count == 0 )); then return 0; fi
  {
    if [[ -f $env_file ]]; then
      while IFS= read -r line || [[ -n $line ]]; do
        if [[ $line =~ $managed && ${changed[${BASH_REMATCH[1]}]+set} ]] ||
          [[ $line =~ $canonical && ${changed[${BASH_REMATCH[2]}]+set} ]]; then
          continue
        fi
        printf '%s\n' "$line"
      done <"$env_file"
    fi
    for key in "${config_keys[@]}"; do
      if [[ ${changed[$key]+set} ]]; then
        printf '%s=%q # marionette-ubuntu-installer\n' "$key" "${config[$key]}"
      fi
    done
  } >"$stage/.env"
}

plan_system_changes() {
  local group
  say '시스템 변경'
  show_command "${elevate[@]}" apt-get update
  show_command "${elevate[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y "${apt_packages[@]}"
  say '  USB udev 규칙은 android-sdk-platform-tools-common 패키지가 설치합니다.'
  for group in "${groups_to_add[@]}"; do
    if ! getent group "$group" >/dev/null; then
      say '  패키지 설치 후 그룹이 없을 때만 다음을 실행합니다.'
      show_command "${elevate[@]}" groupadd --system "$group"
    fi
    show_command "${elevate[@]}" usermod -aG "$group" "$account"
  done
}

install_system_packages() {
  local group
  "${elevate[@]}" apt-get update ||
    die 'APT 패키지 목록 갱신에 실패했습니다. 위 오류의 저장소와 서명 키를 확인하고, sudo apt-get update가 성공한 뒤 설치를 다시 실행하세요.'
  "${elevate[@]}" env DEBIAN_FRONTEND=noninteractive apt-get install -y "${apt_packages[@]}"
  for group in "${groups_to_add[@]}"; do
    if ! getent group "$group" >/dev/null; then
      "${elevate[@]}" groupadd --system "$group"
    fi
    "${elevate[@]}" usermod -aG "$group" "$account"
  done
}

check_kvm() {
  local kvm_ok
  kvm_ok=$(type -P kvm-ok || printf '%s' /usr/sbin/kvm-ok)
  if [[ ! -c /dev/kvm ]]; then
    "$kvm_ok" || true
    die '/dev/kvm을 찾을 수 없습니다. BIOS/UEFI 또는 호스트의 가상화 설정을 확인하세요.'
  fi
  if [[ -r /dev/kvm && -w /dev/kvm ]]; then
    "$kvm_ok" || die 'kvm-ok에 실패했습니다. KVM 설정을 확인하세요.'
  elif [[ " $session_groups " != *' kvm '* && $(stat -c %G /dev/kvm) == kvm ]]; then
    "$kvm_ok" || true
    say '현재 세션에 KVM 그룹이 적용되지 않았습니다. 재로그인 후 kvm-ok를 다시 실행하세요.'
  else
    "$kvm_ok" || true
    die '/dev/kvm 읽기·쓰기 권한이 없습니다. KVM 장치의 권한을 확인하세요.'
  fi
}

download_agent_asset() {
  local url=$1 output=$2 description=$3 status result
  say "$description 다운로드: $url"
  if status=$(curl --disable --fail --silent --show-error --location --retry 2 --connect-timeout 20 \
    --proto '=http,https' --proto-redir '=https' --write-out '%{http_code}' --output "$output" -- "$url"); then
    return 0
  else
    result=$?
  fi
  if [[ $status == 404 || $status == 503 ]]; then
    die "$description 다운로드 실패 (HTTP $status): $url
서버의 에이전트 배포 파일 또는 /downloads/ 프록시 설정을 확인하세요.
서버 관리자는 Linux 패키지 2개와 marionette-agent-linux-SHA256SUMS를 백엔드의 agent-packages 디렉터리에 배치해야 합니다."
  fi
  die "$description 다운로드 실패 (HTTP ${status:-000}, curl $result): $url"
}

stage_package() {
  local hash binary_signature artifact
  if [[ -z $package_dir ]]; then
    download_agent_asset "$backend/downloads/marionette-agent-linux-SHA256SUMS" "$stage/SHA256SUMS" '에이전트 체크섬'
    hash=$(awk -v name="$package.tar.gz" '$2 == name || $2 == "*" name {print $1}' "$stage/SHA256SUMS")
    [[ $hash =~ ^[a-fA-F0-9]{64}$ ]] || die "체크섬 목록에서 $package.tar.gz의 SHA-256을 찾을 수 없습니다. 서버 패키지와 /downloads/ 프록시 설정을 확인하세요."
    download_agent_asset "$backend/downloads/$package.tar.gz" "$stage/$package.tar.gz" '에이전트 패키지'
    ( cd "$stage"; printf '%s  %s.tar.gz\n' "$hash" "$package" | sha256sum --check --status ) ||
      die '다운로드한 에이전트 패키지의 SHA-256이 일치하지 않습니다.'
    tar -xzf "$stage/$package.tar.gz" --no-same-owner --no-same-permissions -C "$stage"
    package_dir=$stage/$package
  fi
  [[ -d $package_dir && ! -L $package_dir ]] || die '패키지 디렉터리가 올바르지 않습니다.'
  for artifact in "${required_files[@]}"; do
    [[ -f $package_dir/$artifact && ! -L $package_dir/$artifact ]] || die "패키지 파일이 없습니다: $artifact"
  done
  binary_signature=$(od -An -tx1 -N 20 -- "$package_dir/marionette-agent" | tr -d '[:space:]')
  [[ $binary_signature == 7f454c460201*"$elf_machine" ]] || die "이 컴퓨터에는 Linux $arch 에이전트 패키지가 필요합니다."

  mkdir "$stage/install"
  for artifact in "${required_files[@]}" "${optional_files[@]}"; do
    [[ -f $package_dir/$artifact && ! -L $package_dir/$artifact ]] || continue
    case "$artifact" in
      marionette-agent|*.sh) install -m 0755 -- "$package_dir/$artifact" "$stage/install/$artifact" ;;
      *) install -m 0644 -- "$package_dir/$artifact" "$stage/install/$artifact" ;;
    esac
  done
}

install_emulator_tools() {
  # Fetch and verify vendor archives as in scripts/setup-tools.py. Stage both
  # tools on the same filesystem and publish directories only when complete.
  # Version 20 still supplies the standalone sdkmanager --licenses command.
  local cmdline_version=20.0
  python3 - "$stage" "$sdk_dir" "$jdk_dir" "$cmdline_version" <<'PY'
import hashlib
import json
from pathlib import Path
import re
import shutil
import stat
import subprocess
import sys
from urllib.parse import urlsplit
import xml.etree.ElementTree as ET
import zipfile

stage, installed_sdk, installed_jdk = map(Path, sys.argv[1:4])
cmdline_version = sys.argv[4]
google = 'https://dl.google.com/android/repository/'
images = google + 'sys-img/google_apis/'
temurin = ('https://api.adoptium.net/v3/assets/latest/17/hotspot'
           '?architecture=x64&image_type=jdk&os=linux&vendor=eclipse')
curl = ['curl', '--disable', '--fail', '--silent', '--show-error', '--location',
        '--retry', '2', '--connect-timeout', '20', '--proto', '=https', '--proto-redir', '=https']


def download(url, path=None):
    if path is None:
        return subprocess.check_output(curl + ['--', url])
    subprocess.run(curl + ['--output', str(path), '--', url], check=True)


def verified_archive(url, checksum, algorithm, filename):
    if algorithm not in ('sha1', 'sha256') or not checksum:
        raise RuntimeError('지원하는 체크섬이 없습니다: ' + url)
    digest = hashlib.new(algorithm)
    if not re.fullmatch(r'[0-9a-fA-F]{%d}' % (2 * digest.digest_size), checksum):
        raise RuntimeError('잘못된 체크섬: ' + url)
    print('다운로드 및 체크섬 확인:', urlsplit(url).path.rsplit('/', 1)[-1], flush=True)
    archive = stage / filename
    download(url, archive)
    with archive.open('rb') as src:
        for chunk in iter(lambda: src.read(1024 * 1024), b''):
            digest.update(chunk)
    if digest.hexdigest() != checksum.lower():
        raise RuntimeError('체크섬이 일치하지 않습니다: ' + url)
    return archive


def unpack_zip(archive, destination):
    destination.mkdir()
    with zipfile.ZipFile(archive) as bundle:
        for member in bundle.infolist():
            target = (destination / member.filename).resolve()
            mode = member.external_attr >> 16
            if destination.resolve() not in target.parents or stat.S_ISLNK(mode):
                raise RuntimeError('안전하지 않은 ZIP 항목: ' + member.filename)
            bundle.extract(member, destination)
            if mode and not member.is_dir():
                target.chmod(mode & 0o777)


def android_package(index, base, name, target, marker):
    for package in index.findall('remotePackage'):
        channel = package.find('channelRef')
        if package.get('path') != name or (channel is not None and channel.get('ref') != 'channel-0'):
            continue
        for archive in package.findall('archives/archive'):
            if archive.findtext('host-os') not in (None, 'linux') or archive.findtext('host-arch') not in (None, 'x86_64', 'x64'):
                continue
            artifact = archive.find('complete')
            if artifact is None:
                continue
            checksum = artifact.find('checksum')
            filename = artifact.findtext('url', '')
            if checksum is None or not re.fullmatch(r'[a-zA-Z0-9_.-]+\.zip', filename):
                continue
            archive_file = verified_archive(base + filename, checksum.text, checksum.get('type') or 'sha1', 'android.zip')
            unpacked = stage / 'android-unpacked'
            unpack_zip(archive_file, unpacked)
            candidates = [path for path in (unpacked, *unpacked.iterdir()) if (path / marker).is_file()]
            if len(candidates) != 1:
                raise RuntimeError('SDK 패키지에 필요한 파일이 없습니다: ' + name)
            target.parent.mkdir(parents=True, exist_ok=True)
            candidates[0].rename(target)
            if unpacked.exists():
                shutil.rmtree(unpacked)
            archive_file.unlink()
            return
    raise RuntimeError('공식 SDK 저장소에서 패키지를 찾지 못했습니다: ' + name)


try:
    if not installed_jdk.exists():
        asset = json.loads(download(temurin))[0]['binary']['package']
        archive = verified_archive(asset['link'], asset['checksum'], 'sha256', 'jdk.tar.gz')
        jdk = stage / 'jdk'
        jdk.mkdir()
        subprocess.run(['tar', '-xzf', str(archive), '--no-same-owner', '--no-same-permissions',
                        '--strip-components=1', '-C', str(jdk)], check=True)
        if not (jdk / 'bin/javac').is_file():
            raise RuntimeError('Temurin JDK에 javac가 없습니다.')
        archive.unlink()

    if not installed_sdk.exists():
        sdk = stage / 'android-sdk'
        repository = ET.fromstring(download(google + 'repository2-1.xml'))
        for name, target, marker in (
            (f'cmdline-tools;{cmdline_version}', f'cmdline-tools/{cmdline_version}', 'bin/sdkmanager'),
            ('platform-tools', 'platform-tools', 'adb'),
            ('emulator', 'emulator', 'emulator'),
        ):
            android_package(repository, google, name, sdk / target, marker)
        index = ET.fromstring(download(images + 'sys-img2-1.xml'))
        android_package(index, images, 'system-images;android-34;google_apis;x86_64',
                        sdk / 'system-images/android-34/google_apis/x86_64', 'system.img')
except (RuntimeError, OSError, ValueError, KeyError, IndexError, ET.ParseError,
        zipfile.BadZipFile, subprocess.CalledProcessError) as exc:
    sys.exit(f'Android 도구 설치 실패: {exc}')
PY
  local sdk=$sdk_dir jdk=$jdk_dir cmdline
  local -a license_status
  if [[ -d $stage/jdk ]]; then jdk=$stage/jdk; fi
  if [[ -d $stage/android-sdk ]]; then
    sdk=$stage/android-sdk
    cmdline=$sdk/cmdline-tools/$cmdline_version/bin
    [[ -x $cmdline/sdkmanager && -x $cmdline/avdmanager &&
      -x $sdk/platform-tools/adb && -x $sdk/emulator/emulator && -x $jdk/bin/java ]] ||
      die '설치한 SDK/JDK 실행 파일을 찾을 수 없습니다.'
    local -a sdkmanager=(env -u ANDROID_SDK_HOME JAVA_HOME="$jdk" ANDROID_HOME="$sdk"
      ANDROID_SDK_ROOT="$sdk" ANDROID_USER_HOME="$stage/android-user"
      "$cmdline/sdkmanager" "--sdk_root=$sdk")
    mkdir "$stage/android-user"
    # yes normally receives SIGPIPE when sdkmanager exits successfully.
    if yes | "${sdkmanager[@]}" --licenses; then
      :
    else
      license_status=("${PIPESTATUS[@]}")
      (( license_status[1] == 0 )) || die 'Android SDK 라이선스를 적용하지 못했습니다.'
    fi
    [[ -s $sdk/licenses/android-sdk-license ]] || die 'Android SDK 라이선스 동의를 확인할 수 없습니다.'
    "${sdkmanager[@]}" --list_installed >"$stage/sdk-packages"
    grep -Eq '^[[:space:]]*system-images;android-34;google_apis;x86_64[[:space:]]+\|' "$stage/sdk-packages" ||
      die 'SDK Manager에서 Android 34 Google APIs 이미지를 찾을 수 없습니다.'
    touch "$sdk/.marionette-ready"
  fi
  if [[ -d $stage/jdk ]]; then mv -T -- "$stage/jdk" "$jdk_dir"; fi
  if [[ -d $stage/android-sdk ]]; then mv -T -- "$stage/android-sdk" "$sdk_dir"; fi
}

prepare_stage() {
  stage=$(mktemp -d "$install_parent/.marionette-agent.XXXXXXXX")
  trap 'rm -rf -- "$stage"' EXIT
}

install_agent() {
  local artifact linger_failed=0
  if [[ ! -d $stage/install ]]; then stage_package; fi
  stage_config
  mkdir -p -- "$install_dir" "$unit_dir" "${config[MARIONETTE_STATE_DIR]}"
  if [[ $mode == emulator ]]; then install_emulator_tools; fi
  if [[ -f $unit_file ]] || systemctl --user is-active --quiet "$unit"; then
    systemctl --user stop "$unit"
  fi
  for artifact in "$stage/install/"* "$stage/install/.env.example"; do
    [[ -f $artifact ]] || continue
    mv -fT -- "$artifact" "$install_dir/${artifact##*/}"
  done
  if [[ -f $stage/.env ]]; then mv -fT -- "$stage/.env" "$env_file"; fi
  chmod 0600 -- "$env_file"
  install -m 0644 -- "$install_dir/$unit" "$unit_file"
  systemctl --user daemon-reload
  systemctl --user enable "$unit"
  if [[ $(loginctl show-user "$account" -p Linger --value) != yes ]]; then
    say '부팅 직후와 로그아웃 후에도 이 사용자 서비스가 실행되도록 linger를 켭니다.'
    show_command "${elevate[@]}" loginctl enable-linger "$account"
    if confirm 'linger를 설정할까요?' && ! "${elevate[@]}" loginctl enable-linger "$account"; then
      say 'linger 설정에 실패했습니다. 위 명령을 다시 실행하면 로그인 전에도 자동 시작할 수 있습니다.' >&2
      linger_failed=1
    fi
  fi
  systemctl --user start "$unit"
  say '로그 보기: journalctl --user -u marionette-agent.service -f'
  if (( relogin )); then
    say '그룹 권한을 적용하려면 로그아웃 후 다시 로그인하세요. 사용자 서비스에 이전 권한이 남으면 재부팅하세요.'
  fi
  say '휴대폰에서 USB 디버깅을 켜고 연결한 뒤 컴퓨터 인증을 허용하세요.'
  say '대시보드의 에이전트 운영에서 연결과 기기 목록을 확인하세요.'
  if [[ $mode == emulator ]]; then say '가상 기기는 대시보드의 운영 설정에서 이미지와 실행 수를 선택하세요.'; fi
  systemctl --user status --no-pager --lines=0 "$unit"
  return "$linger_failed"
}

main() {
  local assume_yes=0 dry_run=0 uninstall=0 purge=0 fallback=0 relogin=0
  local mode_option='' opt key entry account account_groups session_groups arch elf_machine install_lock
  local -a arguments=()
  while (( $# )); do
    opt=$1
    case "$opt" in
      --backend|--id|--token|--mode|--emulators|--tags)
        [[ $# -ge 2 && $2 != --* ]] || die "$opt 옵션에 값이 필요합니다."
        case "$opt" in
          --backend) arguments+=("MARIONETTE_BACKEND_URL=$2") ;;
          --id) arguments+=("MARIONETTE_AGENT_ID=$2") ;;
          --token) arguments+=("MARIONETTE_AGENT_TOKEN=$2") ;;
          --mode) mode_option=$2; [[ $2 == physical || $2 == emulator ]] || die '--mode는 physical 또는 emulator여야 합니다.' ;;
          --emulators) [[ $2 =~ ^[0-8]$ ]] || die '--emulators 값은 0~8이어야 합니다.'; arguments+=("MARIONETTE_MAX_EMULATORS=$2") ;;
          --tags) arguments+=("MARIONETTE_DEVICE_TAGS=$2") ;;
        esac
        shift 2
        ;;
      --yes) assume_yes=1; shift ;;
      --dry-run) dry_run=1; shift ;;
      --uninstall) uninstall=1; shift ;;
      --purge) purge=1; shift ;;
      -h|--help) usage; return 0 ;;
      *) die '알 수 없는 인수입니다. --help를 확인하세요.' ;;
    esac
  done
  (( ! purge || uninstall )) || die '--purge는 --uninstall과 함께 사용하세요.'
  [[ $(uname -s) == Linux && -r /etc/os-release ]] || die 'Ubuntu 또는 Debian에서 실행하세요.'
  # shellcheck source=/dev/null
  source /etc/os-release
  [[ ${ID:-} == ubuntu || ${ID:-} == debian ]] || die 'Ubuntu 또는 Debian에서 실행하세요.'
  (( EUID != 0 )) || die 'sudo로 스크립트를 실행하지 말고 설치할 일반 사용자로 실행하세요.'
  (( BASH_VERSINFO[0] > 4 || ( BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4 ) )) || die 'Bash 4.4 이상이 필요합니다.'
  case "$(uname -m)" in
    x86_64) arch=amd64; elf_machine=3e00 ;;
    aarch64) arch=arm64; elf_machine=b700 ;;
    *) die 'Linux amd64 또는 arm64 컴퓨터가 필요합니다.' ;;
  esac
  [[ $HOME == /* && $HOME != / ]] || die '현재 사용자의 홈 디렉터리를 확인하세요.'

  local install_parent=$HOME/.local/share install_dir=$HOME/.local/share/marionette-agent
  local unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user unit=marionette-agent.service
  local unit_file=$unit_dir/$unit env_file=$install_dir/.env sdk_dir=$install_dir/android-sdk jdk_dir=$install_dir/jdk
  if (( uninstall )); then uninstall_agent; return; fi
  check_install_dir
  if (( ! dry_run )); then lock_installation; fi

  local -a config_keys=(MARIONETTE_BACKEND_URL MARIONETTE_AGENT_ID MARIONETTE_AGENT_TOKEN MARIONETTE_ADB
    MARIONETTE_ANDROID_SDK JAVA_HOME MARIONETTE_STATE_DIR MARIONETTE_MAX_EMULATORS MARIONETTE_DEVICE_TAGS MARIONETTE_ICE_SERVERS)
  local -A inputs=() previous=() config=()
  for key in "${config_keys[@]}"; do
    if [ -v "$key" ]; then inputs[$key]=${!key}; fi
  done
  unset MARIONETTE_AGENT_TOKEN
  for entry in "${arguments[@]}"; do inputs[${entry%%=*}]=${entry#*=}; done
  load_config
  local id_pattern='^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$'
  require_config MARIONETTE_BACKEND_URL '백엔드 URL'
  require_config MARIONETTE_AGENT_ID '에이전트 ID'
  require_config MARIONETTE_AGENT_TOKEN '에이전트 토큰'
  validate_config

  local mode=${mode_option:-physical}
  if [[ $mode == emulator && $arch == arm64 ]]; then
    say 'Linux arm64용 공식 Android Emulator가 제공되지 않습니다. 실물 기기용으로 설치합니다.'
    mode=physical
    fallback=1
  fi
  if (( fallback )); then
    config[MARIONETTE_MAX_EMULATORS]=0
  elif [[ $mode_option == physical && ! ${inputs[MARIONETTE_MAX_EMULATORS]+set} ]]; then
    config[MARIONETTE_MAX_EMULATORS]=0
  elif [[ $mode == emulator && ! ${inputs[MARIONETTE_MAX_EMULATORS]+set} &&
    ${previous[MARIONETTE_MAX_EMULATORS]:-0} == 0 && ${previous[MARIONETTE_ANDROID_SDK]:-} != "$sdk_dir" ]]; then
    config[MARIONETTE_MAX_EMULATORS]=2
  fi
  if [[ ! ${config[MARIONETTE_MAX_EMULATORS]:-} ]]; then
    if [[ $mode == emulator || -f $env_file ]]; then
      config[MARIONETTE_MAX_EMULATORS]=2
    else
      config[MARIONETTE_MAX_EMULATORS]=0
    fi
  fi
  configure_paths
  if (( ! dry_run )); then check_saved_emulator_count; fi

  local package=marionette-agent-linux-$arch package_dir='' backend=${config[MARIONETTE_BACKEND_URL]:-'<URL>'}
  backend=${backend%/}
  if [[ -n $source_script && -f $source_script ]]; then
    package_dir=$(cd "$(dirname -- "$source_script")" && pwd -P)
    if [[ ! -f $package_dir/marionette-agent ]]; then package_dir=''; fi
  fi
  local -a required_files=(marionette-agent marionette-device.jar start-agent.sh setup-ubuntu-agent.sh "$unit")
  local -a optional_files=(.env.example README.md UBUNTU_AGENT.md AGENT_INSTALL.md PROTOCOL.md AUTOMATION.md API_MCP.md)
  local -a apt_packages=(adb android-sdk-platform-tools-common ca-certificates curl)
  local -a groups_to_add=() required_groups=(plugdev) elevate=(sudo)
  if (( assume_yes )); then elevate+=(-n); fi
  if [[ $mode == emulator ]]; then
    apt_packages+=(cpu-checker qemu-system-x86 python3)
    required_groups+=(kvm)
    if (( ! dry_run )); then
      grep -Eq '^flags[[:blank:]]*:.*[[:blank:]](vmx|svm)([[:blank:]]|$)' /proc/cpuinfo ||
        die 'CPU 가상화(vmx/svm)가 보이지 않습니다. BIOS/UEFI 설정을 확인하세요.'
      [[ ! -L $sdk_dir && ! -L $jdk_dir ]] || die '전용 SDK/JDK 폴더의 심볼릭 링크를 해제한 뒤 다시 실행하세요.'
      [[ ! -e $sdk_dir || -f $sdk_dir/.marionette-ready ]] || die "기존 SDK를 보호하기 위해 설치를 중단했습니다: $sdk_dir"
      [[ ! -e $jdk_dir || -x $jdk_dir/bin/javac ]] || die "기존 JDK 폴더를 확인하세요: $jdk_dir"
    fi
    say 'Android SDK 라이선스를 확인하세요. https://developer.android.com/studio/terms'
    say '에뮬레이터를 처음 설치할 때 sdkmanager --licenses의 질문에 자동으로 동의합니다.'
    if (( ! dry_run )) && [[ ! -f $sdk_dir/.marionette-ready ]]; then
      confirm '라이선스 자동 동의와 에뮬레이터 설치를 진행할까요?' || die '에뮬레이터 설치를 취소했습니다.'
    fi
  fi
  account=$(id -un)
  account_groups=$(id -nG "$account")
  session_groups=$(id -nG)
  local group
  for group in "${required_groups[@]}"; do
    if [[ " $account_groups " != *" $group "* ]]; then groups_to_add+=("$group"); fi
    if [[ " $session_groups " != *" $group "* ]]; then relogin=1; fi
  done
  plan_system_changes

  if (( dry_run )); then
    say '변경 없이 설치 계획을 표시합니다.'
    say "설치 잠금 및 임시 작업 공간 준비: $install_parent"
    if [[ -n $package_dir ]]; then
      say "이 패키지에서 에이전트 파일 복사: $package_dir"
    else
      say "다운로드 후 SHA-256 확인 및 압축 해제: $backend/downloads/$package.tar.gz"
      say "체크섬 목록: $backend/downloads/marionette-agent-linux-SHA256SUMS"
    fi
    if [[ $mode == emulator ]]; then
      say 'CPU 가상화와 /dev/kvm 권한 확인 (kvm-ok)'
      say "Google 저장소 체크섬 검증 및 SDK 설치: $sdk_dir"
      say "Adoptium SHA-256 검증 및 Temurin JDK 17 설치: $jdk_dir"
      say '새 SDK에서만 sdkmanager --licenses 및 설치 이미지 확인'
    fi
    say "설치 파일 경로: $install_dir"
    say "상태 디렉터리 준비 (기존 설정 우선): ${config[MARIONETTE_STATE_DIR]}"
    say ".env 값 병합 및 권한 0600 적용: $env_file"
    say "기존 서비스를 중지한 뒤 사용자 유닛 설치: $unit_file"
    show_command systemctl --user daemon-reload
    show_command systemctl --user enable "$unit"
    say '  linger가 꺼져 있을 때만 다음을 실행합니다.'
    show_command "${elevate[@]}" loginctl enable-linger "$account"
    show_command systemctl --user start "$unit"
    show_command systemctl --user status --no-pager --lines=0 "$unit"
    return 0
  fi

  check_user_systemd
  confirm '시스템 변경을 진행할까요?' || die '설치를 취소했습니다.'
  prepare_stage
  if [[ -n $package_dir ]] || command -v curl >/dev/null 2>&1; then stage_package; fi
  install_system_packages
  if [[ $mode == emulator ]]; then check_kvm; fi
  install_agent
}

# Keep commands from consuming the installation script when Bash reads it from stdin.
main "$@" </dev/null
