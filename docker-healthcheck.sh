#!/bin/sh
set -eu

listen=${HARMONIA_LISTEN:-:8090}
# Connect to loopback for wildcard listeners; preserve explicit bind addresses.
case "$listen" in
  :*) listen="127.0.0.1$listen" ;;
  0.0.0.0:*) listen="127.0.0.1:${listen##*:}" ;;
  \[::\]:*) listen="[::1]:${listen##*:}" ;;
esac

exec wget -Y off -q -T 2 -O /dev/null "http://$listen/api/health"
