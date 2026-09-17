#!/bin/sh

configured_steamcmd_paths() {
  awk -F= '
    /^[[:space:]]*(STEAMCMD_PATH|STEAM_CMD_PATH)[[:space:]]*=/ {
      value=$2
      sub(/^[[:space:]]+/, "", value)
      sub(/[[:space:]]+$/, "", value)
      if (value != "") print value
    }
  ' "$1" | sort -u
}

discover_steamcmd() {
  if command -v steamcmd >/dev/null 2>&1; then
    command -v steamcmd
    return 0
  fi
  for candidate in \
    /usr/games/steamcmd \
    /usr/bin/steamcmd \
    /opt/steamcmd/steamcmd.sh \
    /opt/dst/steamcmd/steamcmd.sh
  do
    if [ -x "$candidate" ]; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

ensure_configured_steamcmd() {
  config_path=$1
  configured_steamcmd_paths "$config_path" | while IFS= read -r configured_path
  do
    case "$configured_path" in
      /*) ;;
      *) echo "SteamCMD path must be absolute: $configured_path" >&2; exit 70 ;;
    esac
    if [ -x "$configured_path" ]; then
      continue
    fi
    if [ -e "$configured_path" ]; then
      echo "configured SteamCMD is not executable: $configured_path" >&2
      exit 70
    fi
    resolved_path=$(discover_steamcmd) || {
      echo "SteamCMD is configured as $configured_path but no executable SteamCMD was found" >&2
      exit 70
    }
    if [ "$resolved_path" = "$configured_path" ]; then
      continue
    fi
    mkdir -p "$(dirname "$configured_path")"
    chmod 0755 "$(dirname "$configured_path")"
    ln -s "$resolved_path" "$configured_path"
    echo "created stable SteamCMD entry: $configured_path -> $resolved_path"
  done
}
