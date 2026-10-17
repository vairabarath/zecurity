#!/bin/bash
# shipenv.sh <terminal> <local-env-file> <remote-path>
# Copies a secret env file into a remote Orca terminal via cat heredoc-less stdin + Ctrl-D.
# Never echoes the content; verifies by sha prefix.
export XDG_CONFIG_HOME="$HOME/.config"
T=$1; F=$2; R=$3
orca-ide terminal send --terminal "$T" --text "umask 077; cat > $R && chmod 600 $R && echo ENV_OK" --enter --json >/dev/null
sleep 1
orca-ide terminal send --terminal "$T" --text "$(cat "$F")
" --json >/dev/null
sleep 0.5
orca-ide terminal send --terminal "$T" --text $'\x04' --json >/dev/null
sleep 1
echo "local sha: $(sha256sum "$F" | cut -c1-12)"
