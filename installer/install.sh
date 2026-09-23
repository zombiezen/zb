#!/usr/bin/env bash
# Copyright 2025 The zb Authors
# SPDX-License-Identifier: MIT

# The zb Unix installer.
# Run ./install --help for information.

set -euo pipefail

log() {
  echo "$@" >&2
}
to_abs() {
  if [[ -z "$1" ]]; then
    echo ""
  else
    cd -- "$1" &> /dev/null && pwd
  fi
}

if [[ "${ZB_STORE_DIR:-/opt/zb/store}" != /opt/zb/store ]]; then
  log "ZB_STORE_DIR must be either unset or set to the default, /opt/zb/store."
  log "The store objects in this release use the default path."
  exit 1
fi
export ZB_STORE_DIR="/opt/zb/store"

isMacOS=0
isLinux=0
case "$(uname -s)" in
  Darwin)
    isMacOS=1
    ;;
  Linux)
    isLinux=1
    ;;
esac

installer_dir="$( to_abs "$( dirname -- "${BASH_SOURCE[0]}" )" )"
bin_dir=/usr/local/bin
bin_dir_explicit=0
single_user=0
install_units="$isLinux"
install_launchdaemon="$isMacOS"
build_users_group="zbld"
build_gid=256000
if [[ "$isMacOS" -eq 1 ]]; then
  # As per https://serverfault.com/a/390671,
  # must be <500 to be a "system" group and thus hidden in System Settings.
  # 0-304 are effectively reserved, as are 400-500.
  # Nix uses 350.
  build_gid=356
fi
first_build_uid=256001
build_user_count=32
usage() {
  log "usage: $0 [options]"
  log
  log "    --single-user               install without root privileges"
  log "    --bin DIR                   create symlinks to binaries in the given directory (default $bin_dir)"
  # TODO(now): Gut this.
  log "    --build-users-group NAME    use the given Unix group for running builds, creating if necessary (default $build_users_group)"
  log "    --build-gid GID             group ID of Unix group to use if creating (default $build_gid)"
  log "    --build-users N             create N build users if creating build group (default $build_user_count)"
  log "    --first-build-uid UID       ID of first build user if creating build group (default $first_build_uid)"
  log "    --systemd                   install systemd units (default to yes on Linux)"
  log "    --no-systemd                do not install systemd units"
  log "    --launchd                   install launchd daemon (default to yes on macOS)"
  log "    --no-launchd                do not install launchd daemon"
  log "    --installer-dir DIR         install resources from the given directory (default $installer_dir)"
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --installer-dir)
      installer_dir="$( to_abs "$2" )"
      shift 2
      ;;
    --bin)
      bin_dir="$( to_abs "$2" )"
      bin_dir_explicit=1
      shift 2
      ;;
    --single-user)
      single_user=1
      shift
      ;;
    --build-users-group)
      build_users_group="$2"
      shift 2
      ;;
    --build-gid)
      build_gid="$2"
      shift 2
      ;;
    --first-build-uid)
      first_build_uid="$2"
      shift 2
      ;;
    --build-users)
      build_user_count="$2"
      shift 2
      ;;
    --systemd)
      install_units=1
      shift
      ;;
    --no-systemd)
      install_units=0
      shift
      ;;
    --launchd)
      install_launchdaemon=1
      shift
      ;;
    --no-launchd)
      install_launchdaemon=0
      shift
      ;;
    --)
      shift
      break
      ;;
    --*)
      usage
      if [[ "$1" = '--help' ]]; then
        exit 0
      else
        exit 64
      fi
      ;;
    *)
      break
  esac
done

if [[ $# -gt 0 ]]; then
  usage
  exit 64
fi
# Clear bin directory if --single-user given without --bin.
if [[ $bin_dir_explicit -eq 0 && $single_user -eq 1 ]]; then
  bin_dir=''
fi
# Clear build group if --single-user is given.
if [[ $single_user -eq 1 ]]; then
  build_users_group=''
fi

zb_object="$( cd "$installer_dir/store" > /dev/null && echo *-zb-* )"
if [[ "$zb_object" = '*-zb-*' ]]; then
  zb_object="$( cd "$installer_dir/store" > /dev/null && echo *-zb )"
  if [[ "$zb_object" = '*-zb' ]]; then
    log "Error: missing zb object from $installer_dir/store"
    exit 1
  fi
fi
registry="$installer_dir/registry.txt"
if [[ ! -e "$registry" ]]; then
  log "Error: missing $registry"
  exit 1
fi

if [[ single_user -eq 1 || "$(id -u)" -eq 0 ]]; then
  run_as_target_user() {
    "$@"
  }
else
  run_as_target_user() {
    sudo --non-interactive -- "$@"
  }
  log "Testing for password-less sudo..."
  if ! run_as_target_user true ; then
    log "Please re-run this installer as root or with --single-user."
    exit 1
  fi
  log "Success. Running as root."
  if [[ -t 2 ]]; then
    log "Ctrl-C to stop the install process if this is not what you meant to do."
    sleep 5
  fi
fi

log "Creating ${ZB_STORE_DIR}..."
run_as_target_user mkdir -p "$ZB_STORE_DIR"
run_as_target_user chmod 1775 "$ZB_STORE_DIR"

for i in $( cd "$installer_dir/store" > /dev/null && echo * ); do
  dst="$ZB_STORE_DIR/$i"
  if [[ -e "$dst" ]]; then
    continue
  fi
  log "Copying $dst..."
  temp_dst="${dst}~"
  if [[ -e "$temp_dst" ]]; then
    run_as_target_user rm -rf "$temp_dst"
  fi
  run_as_target_user cp -RPp "$installer_dir/store/$i" "$temp_dst"
  run_as_target_user find "$temp_dst" -exec touch -m -h -d '1970-01-01T00:00:00Z' '{}' \;
  run_as_target_user chmod -R a-w "$temp_dst"
  run_as_target_user mv "$temp_dst" "$dst"
done

zb="$ZB_STORE_DIR/$zb_object/bin/zb"
log "Initializing store database..."
run_as_target_user "$zb" store object register < "$registry"

if [[ -z "$bin_dir" ]]; then
  log "zb installed at $zb"
  if [[ "$bin_dir_explicit" -eq 0 ]]; then
    log "Add $(dirname "$zb") to your PATH to complete installation."
  fi
else
  log "Adding symlinks to ${bin_dir}..."
  run_as_target_user ln -sf "$zb" "$bin_dir/zb"
fi

if [[ -n "$build_users_group" ]]; then
  if [[ "$isLinux" -eq 1 ]]; then
    if getent group "$build_users_group" > /dev/null; then
      log "Reusing existing group $build_users_group"
    else
      log "Adding group $build_users_group"
      run_as_target_user groupadd \
        --gid "$build_gid" \
        -- "$build_users_group"
      for i in $( seq "$build_user_count" ); do
        build_user_name="${build_users_group}${i}"
        log "Adding user $build_user_name"
        run_as_target_user useradd \
          --uid $(( first_build_uid + i - 1 )) \
          --gid "$build_gid" \
          --groups "$build_users_group" \
          --comment "zb build user $i" \
          --no-user-group \
          --system \
          --no-create-home \
          --shell /usr/sbin/nologin \
          --password '!' \
          -- "$build_user_name"
      done
    fi
  elif [[ "$isMacOS" -eq 1 ]]; then
    if dscl . -read "/Groups/$build_users_group" >& /dev/null; then
      log "Reusing existing group $build_users_group"
    else
      log "Adding group $build_users_group"
      run_as_target_user dseditgroup \
        -o create \
        -r "zb build user group" \
        -i "$build_gid" \
        -- "$build_users_group" >&2
      for i in $( seq "$build_user_count" ); do
        build_user_name="${build_users_group}${i}"
        log "Adding user $build_user_name"
        run_as_target_user dscl . create "/Users/$build_user_name" \
          UniqueID $(( first_build_uid + i - 1 ))
        run_as_target_user dscl . create "/Users/$build_user_name" \
          IsHidden 1
        run_as_target_user dscl . create "/Users/$build_user_name" \
          NFSHomeDirectory /var/empty
        run_as_target_user dscl . create "/Users/$build_user_name" \
          RealName "zb build user $i"
        run_as_target_user dscl . create "/Users/$build_user_name" \
          UserShell /usr/bin/false
        run_as_target_user dscl . create "/Users/$build_user_name" \
          PrimaryGroupID "$build_gid"

        run_as_target_user dseditgroup \
          -o edit \
          -t user \
          -a "$build_user_name" \
          -- "$build_users_group"
      done
    fi
  else
    log "Do not know how to create groups on $(uname -s)"
  fi

  run_as_target_user chown ":$build_users_group" "$ZB_STORE_DIR"
fi

if [[ "$install_units" -eq 1 ]]; then
  if [[ "$single_user" -eq 1 ]]; then
    systemd_install_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  else
    systemd_install_dir="/etc/systemd/system"
  fi
  run_as_target_user mkdir -p "$systemd_install_dir"

  zb_systemd="$ZB_STORE_DIR/$zb_object/lib/systemd/system"
  log "Installing ${systemd_install_dir}/zb-serve.socket..."
  run_as_target_user ln -sf "$zb_systemd/zb-serve.socket" "$systemd_install_dir/zb-serve.socket"
  log "Installing ${systemd_install_dir}/zb-serve.service..."
  run_as_target_user ln -sf "$zb_systemd/zb-serve.service" "$systemd_install_dir/zb-serve.service"
  if [[ "$build_users_group" != zbld || "$single_user" -eq 1 ]]; then
    run_as_target_user mkdir -p "$systemd_install_dir/zb-serve.service.d"
    {
      echo '# File managed by the zb installer.'
      echo "[Service]"
      echo "Environment=ZB_BUILD_USERS_GROUP=$build_users_group"
      if [[ "$single_user" -eq 1 ]]; then
        echo "Environment=ZB_SERVE_FLAGS=--sandbox=0"
      fi
    } | run_as_target_user tee "$systemd_install_dir/zb-serve.service.d/00-installer.conf" > /dev/null
  else
    run_as_target_user rm -f "$systemd_install_dir/zb-serve.service.d/00-installer.conf"
  fi

  if [[ "$single_user" -eq 1 ]]; then
    run_as_target_user systemctl --user daemon-reload
    run_as_target_user systemctl --user enable zb-serve.socket zb-serve.service
    run_as_target_user systemctl --user restart zb-serve.service
  else
    run_as_target_user systemctl daemon-reload
    run_as_target_user systemctl enable zb-serve.socket zb-serve.service
    run_as_target_user systemctl restart zb-serve.service
  fi
fi

if [[ "$install_launchdaemon" -eq 1 ]]; then
  if [[ "$single_user" -eq 1 ]]; then
    launchd_install_dir="$HOME/Library/LaunchAgents"
  else
    launchd_install_dir="/Library/LaunchDaemons"
  fi
  run_as_target_user mkdir -p "$launchd_install_dir"

  log "Installing ${launchd_install_dir}/dev.zb-build.serve.plist..."
  run_as_target_user cp \
    "$ZB_STORE_DIR/$zb_object/Library/LaunchDaemons/dev.zb-build.serve.plist" \
    "$launchd_install_dir/dev.zb-build.serve.plist"
  if [[ "$build_users_group" != zbld ]]; then
    run_as_target_user defaults write \
      "$launchd_install_dir/dev.zb-build.serve.plist" \
      ProgramArguments \
      -array-add "--build-users-group=$build_users_group"
  fi

  run_as_target_user launchctl load -w "$launchd_install_dir/dev.zb-build.serve.plist"
fi

log "Installation complete."
